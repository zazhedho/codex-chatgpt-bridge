package main

import (
	"log"

	"example.com/codex-chatgpt-bridge/internal/app"
)

func main() {
	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
