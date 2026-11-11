package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/riggleg/mosso/internal/server"
)

func main() {
	addr := flag.String("listen", "127.0.0.1:8080", "HTTP listen address")
	flag.Parse()

	log.Printf("mosso listening on %s", *addr)
	if err := http.ListenAndServe(*addr, server.NewHandler()); err != nil {
		log.Fatal(err)
	}
}
