package main

import (
	"os"

	"github.com/dongho-jung/myriad/internal/myriad"
)

func main() {
	os.Exit(myriad.Run(os.Args[1:]))
}
