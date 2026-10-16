// Command wfdebug serves the local workflow debugger: an SQLite backed
// Infrastructure over HTTP plus a web UI.
//
//	go run ./workflow/cmd/wfdebug -addr 127.0.0.1:7788 -db wfdebug.db
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/k0923/go/workflow/debugger"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7788", "listen address")
	db := flag.String("db", "wfdebug.db", "SQLite database path (':memory:' for ephemeral)")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := debugger.ListenAndServe(ctx, *addr, *db); err != nil {
		log.Fatal(err)
	}
}
