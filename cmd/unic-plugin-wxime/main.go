package main

import (
	"os"

	wxime "github.com/mkdir700/unic-plugin-wxime"
	"github.com/uniclipboard/unic/plugin"
)

func main() {
	if plugin.Serve(wxime.New()) != nil {
		os.Exit(1)
	}
}
