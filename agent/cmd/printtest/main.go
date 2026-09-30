package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/printbridge/printbridge/agent/pkg/printer"
)

func main() {
	listFlag := flag.Bool("list", false, "List installed system printers")
	printerNameFlag := flag.String("printer", "", "Target printer name for raw print test")
	netFlag := flag.String("net", "", "Send the test payload straight to a network printer's RAW port (host or host:port, default port 9100)")
	probeFlag := flag.Bool("probe", false, "Only check whether the network printer accepts a TCP connection")
	escposFlag := flag.Bool("escpos", false, "Send standard ESC/POS thermal test print with cut command")
	zplFlag := flag.Bool("zpl", false, "Send standard ZPL sample label test print")
	customTextFlag := flag.String("text", "", "Send raw text string to target printer")

	flag.Parse()

	if *listFlag || len(os.Args) == 1 {
		fmt.Println("=== PrintBridge System Printer Discovery ===")
		printers, err := printer.ListPrinters()
		if err != nil {
			log.Fatalf("Failed to list printers: %v", err)
		}

		if len(printers) == 0 {
			fmt.Println("No printers found on system.")
			return
		}

		for i, p := range printers {
			defaultMark := ""
			if p.IsDefault {
				defaultMark = " (DEFAULT)"
			}
			fmt.Printf("[%d] %s%s\n", i+1, p.Name, defaultMark)
			fmt.Printf("    Driver: %s | Port: %s | Status: %s | Online: %t\n\n",
				p.DriverName, p.PortName, p.StatusDescription, p.IsOnline)
		}
		return
	}

	if *printerNameFlag == "" && *netFlag == "" {
		log.Fatalf("Please specify -printer \"Printer Name\" or -net host[:port]")
	}

	// A pure reachability check needs no payload: it answers the question an
	// installer actually has, "is the printer there at all?".
	if *probeFlag {
		if *netFlag == "" {
			log.Fatalf("-probe requires -net host[:port]")
		}

		target, ok := printer.ParseNetworkPort(*netFlag)
		if !ok {
			log.Fatalf("Invalid network target %q (expected host or host:port)", *netFlag)
		}

		if err := printer.ProbeNetworkTarget(target, 3*time.Second); err != nil {
			log.Fatalf("PROBE FAILED: %v", err)
		}

		fmt.Printf("SUCCESS: %s accepted a TCP connection on its RAW port\n", target.Address())
		return
	}

	var payload []byte
	var jobType string

	if *escposFlag {
		jobType = "ESC/POS Test"
		// Init (\x1B\x40) + Centered Bold Title + Text + Paper Cut (\x1DV\x41\x00)
		payload = []byte{
			0x1B, 0x40, // ESC @ (Initialize printer)
			0x1B, 0x61, 0x01, // ESC a 1 (Center align)
			0x1B, 0x45, 0x01, // ESC E 1 (Bold ON)
			'P', 'r', 'i', 'n', 't', 'B', 'r', 'i', 'd', 'g', 'e', '\n',
			0x1B, 0x45, 0x00, // ESC E 0 (Bold OFF)
			0x1B, 0x61, 0x00, // ESC a 0 (Left align)
			'R', 'a', 'w', ' ', 'E', 'S', 'C', '/', 'P', 'O', 'S', ' ', 'P', 'a', 's', 's', 't', 'h', 'r', 'o', 'u', 'g', 'h', ' ', 'T', 'e', 's', 't', '\n',
			'\n', '\n', '\n',
			0x1D, 0x56, 0x41, 0x00, // GS V 65 0 (Full Cut)
		}
	} else if *zplFlag {
		jobType = "ZPL Label Test"
		payload = []byte("^XA^FO50,50^A0N,50,50^FDPrintBridge ZPL Test^FS^FO50,120^BQN,2,5^FDQA,PrintBridge Direct Spooler^FS^XZ\n")
	} else if *customTextFlag != "" {
		jobType = "Custom Text"
		payload = []byte(*customTextFlag + "\n")
	} else {
		log.Fatalf("Must specify one of -escpos, -zpl, or -text \"content\"")
	}

	if *netFlag != "" {
		target, ok := printer.ParseNetworkPort(*netFlag)
		if !ok {
			log.Fatalf("Invalid network target %q (expected host or host:port)", *netFlag)
		}

		if err := printer.SendRawToNetwork(target.Address(), target, payload, 10*time.Second); err != nil {
			log.Fatalf("SendRawToNetwork failed: %v", err)
		}

		fmt.Printf("SUCCESS: %s (%d bytes) written straight to %s - no spooler, no driver\n", jobType, len(payload), target.Address())
		return
	}

	fmt.Printf("Sending %s (%d bytes) directly to printer %q...\n", jobType, len(payload), *printerNameFlag)

	err := printer.PrintRaw(*printerNameFlag, payload, jobType)
	if err != nil {
		log.Fatalf("PrintRaw failed: %v", err)
	}

	fmt.Println("SUCCESS: Raw print job submitted to OS print spooler with zero print dialogs!")
}
