// Minimal host program for Soda: it uses upstream PocketBase as a
// library and registers Soda's Ramune-backed JSVM plugin. Drop-in
// replacement for github.com/pocketbase/pocketbase/plugins/jsvm.
package main

import (
	"log"
	"os"

	"github.com/pocketbase/pocketbase"

	"github.com/i2y/soda"
)

func main() {
	app := pocketbase.New()

	var hooksDir string
	app.RootCmd.PersistentFlags().StringVar(&hooksDir, "hooksDir", "", "override the JS hooks directory (defaults to pb_data/../pb_hooks)")

	var hooksWatch bool
	app.RootCmd.PersistentFlags().BoolVar(&hooksWatch, "hooksWatch", true, "auto-restart on pb_hooks file change; no effect on Windows")

	app.RootCmd.ParseFlags(os.Args[1:])

	soda.MustRegister(app, soda.Config{
		HooksDir:   hooksDir,
		HooksWatch: hooksWatch,
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
