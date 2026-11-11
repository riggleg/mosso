package main

import (
	"flag"
	"log"
)

func main() {
	sessionID := flag.String("session", "", "Mosso session ID (reserved for worker integration)")
	flag.Parse()
	if *sessionID == "" {
		log.Print("mosso-worker skeleton: Pi RPC and control channel are not implemented")
		return
	}
	log.Printf("mosso-worker skeleton for session %q: not yet implemented", *sessionID)
}
