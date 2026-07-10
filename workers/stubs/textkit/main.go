package main

import (
	"os"
	"strconv"

	crucible "github.com/Unluckyathecking/crucible/workers/sdk-go"
	"github.com/Unluckyathecking/crucible/workers/stubs/textkit/handler"
)

func main() {
	port := 8082
	if v := os.Getenv("PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			port = n
		}
	}
	if err := crucible.Serve(port, handler.Handle); err != nil {
		panic(err)
	}
}
