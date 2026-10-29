package app

import (
	"errors"
	"flag"
	"log"
	"os"

	"github.com/dkroshow/nano/internal/config"
)

// Main runs Nano using the process arguments and standard streams.
func Main() {
	cfg, err := config.Parse(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		log.Fatal(err)
	}
	run(cfg)
}

func run(cfg config.Config) {
	application := construct(cfg)
	local, remote := application.mount()
	application.run(local, remote)
}
