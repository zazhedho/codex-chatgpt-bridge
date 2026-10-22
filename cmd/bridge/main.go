package main

import (
	"log"

	"github.com/zazhedho/codex-chatgpt-bridge/internal/app"
)

func main() {
	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
