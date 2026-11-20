// Command passthrough plays what a capture device records on a playback
// device, or lists the devices with -list. Use headphones: there is no echo
// cancellation.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	koloaudio "github.com/ciranda-dev/kolo-audio"
)

func main() {
	list := flag.Bool("list", false, "list devices and exit")
	inID := flag.String("in", "", "capture device ID (empty: system default)")
	outID := flag.String("out", "", "playback device ID (empty: system default)")
	seconds := flag.Int("seconds", 10, "how long to run")
	flag.Parse()

	if *list {
		for _, kind := range []koloaudio.Kind{koloaudio.Capture, koloaudio.Playback} {
			devs, err := koloaudio.Devices(kind)
			if err != nil {
				log.Fatal(err)
			}
			for _, d := range devs {
				mark := " "
				if d.Default {
					mark = "*"
				}
				fmt.Printf("%-8s %s %-40q %s\n", kind, mark, d.Name, d.ID)
			}
		}
		return
	}

	if err := run(*inID, *outID, *seconds); err != nil {
		log.Fatal(err)
	}
}

func run(inID, outID string, seconds int) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := koloaudio.RequestAccess(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()

	in, err := koloaudio.OpenInput(koloaudio.Config{DeviceID: inID})
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := koloaudio.OpenOutput(koloaudio.Config{DeviceID: outID})
	if err != nil {
		return err
	}
	defer out.Close()

	buf := make([]int16, 960)
	for ctx.Err() == nil {
		n, err := in.Read(ctx, buf)
		if err == nil {
			_, err = out.Write(ctx, buf[:n])
		}
		if err != nil && ctx.Err() == nil {
			return err
		}
	}
	fmt.Printf("input  %+v\noutput %+v\n", in.Stats(), out.Stats())
	return nil
}
