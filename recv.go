// RTLAMR - An rtl-sdr receiver for smart meters operating in the 900MHz ISM band.
// Copyright (C) 2015 Douglas Hall
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/bemasher/rtlamr/idm"
	"github.com/bemasher/rtlamr/parse"
	"github.com/bemasher/rtlamr/r900"
	"github.com/bemasher/rtlamr/scm"
	"github.com/jpoirier/gortlsdr"
)

var rcvr Receiver

type Receiver struct {
	*rtlsdr.Context
	p  parse.Parser
	fc parse.FilterChain
}

func (rcvr *Receiver) NewReceiver() {
	switch strings.ToLower(*msgType) {
	case "scm":
		rcvr.p = scm.NewParser(*symbolLength, *decimation)
	case "idm":
		rcvr.p = idm.NewParser(*symbolLength, *decimation)
	case "r900":
		rcvr.p = r900.NewParser(*symbolLength, *decimation)
	default:
		log.Fatalf("Invalid message type: %q\n", *msgType)
	}

	if !*quiet {
		rcvr.p.Log()
	}

	// Open rtl-sdr dongle.
	var err error
	if rcvr.Context, err = rtlsdr.Open(0); err != nil {
		log.Fatal(err)
	}

	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "unique":
			rcvr.fc.Add(NewUniqueFilter())
		case "filterid":
			rcvr.fc.Add(meterID)
		case "filtertype":
			rcvr.fc.Add(meterType)
		}
	})

	if err := rcvr.SetCenterFreq(int(rcvr.p.Cfg().CenterFreq)); err != nil {
		log.Fatal(err)
	}
	if err := rcvr.SetSampleRate(int(rcvr.p.Cfg().SampleRate)); err != nil {
		log.Fatal(err)
	}
	if err := rcvr.SetTunerGainMode(false); err != nil {
		log.Fatal(err)
	}

	log.Println(rcvr.GetCenterFreq())
	log.Println(rcvr.GetSampleRate())

	rcvr.ResetBuffer()

	return
}

func (rcvr *Receiver) Run() {
	// Setup signal channel for interruption.
	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, os.Kill, os.Interrupt)

	// Setup time limit channel
	tLimit := make(<-chan time.Time, 1)
	if *timeLimit != 0 {
		tLimit = time.After(*timeLimit)
	}

	in, out := io.Pipe()
	userCtx := rtlsdr.UserCtx(out)

	defer func() {
		in.Close()
		out.Close()
	}()

	ctx := &rtlsdr.CustUserCtx{
		func(buf []byte, ctx *rtlsdr.UserCtx) {
			out := (*ctx).(*io.PipeWriter)
			out.Write(buf)
		},
		&userCtx,
	}

	go rcvr.ReadAsync2(ctx, 1, 16384)

	block := make([]byte, rcvr.p.Cfg().BlockSize2)

	start := time.Now()
	for {
		// Exit on interrupt or time limit, otherwise receive.
		select {
		case <-sigint:
			return
		case <-tLimit:
			fmt.Println("Time Limit Reached:", time.Since(start))
			return
		default:
			// Read new sample block.
			_, err := io.ReadFull(in, block)
			if err != nil {
				log.Fatal("Error reading samples: ", err)
			}

			pktFound := false
			indices := rcvr.p.Dec().Decode(block)

			for _, pkt := range rcvr.p.Parse(indices) {
				if !rcvr.fc.Match(pkt) {
					continue
				}

				var msg parse.LogMessage
				msg.Time = time.Now()
				msg.Offset, _ = sampleFile.Seek(0, os.SEEK_CUR)
				msg.Length = rcvr.p.Cfg().BufferLength << 1
				msg.Message = pkt

				err = encoder.Encode(msg)
				if err != nil {
					log.Fatal("Error encoding message: ", err)
				}

				// The XML encoder doesn't write new lines after each
				// element, add them.
				if _, ok := encoder.(*xml.Encoder); ok {
					fmt.Fprintln(logFile)
				}

				pktFound = true
				if *single {
					if len(meterID.UintMap) == 0 {
						break
					} else {
						delete(meterID.UintMap, uint(pkt.MeterID()))
					}
				}
			}

			if pktFound {
				if *sampleFilename != os.DevNull {
					_, err = sampleFile.Write(rcvr.p.Dec().IQ)
					if err != nil {
						log.Fatal("Error writing raw samples to file:", err)
					}
				}
				if *single && len(meterID.UintMap) == 0 {
					return
				}
			}
		}
	}
}

func init() {
	log.SetFlags(log.Lshortfile | log.Lmicroseconds)
}

func main() {
	RegisterFlags()

	flag.Parse()
	HandleFlags()

	rcvr.NewReceiver()

	defer func() {
		logFile.Close()
		sampleFile.Close()

		fmt.Println("Cancelling...")
		err := rcvr.CancelAsync()
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("Closing...")
		rcvr.Close()
		os.Exit(0)
	}()

	rcvr.Run()
}
