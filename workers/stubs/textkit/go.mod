module github.com/Unluckyathecking/crucible/workers/stubs/textkit

go 1.25.0

toolchain go1.25.11

require github.com/Unluckyathecking/crucible/workers/sdk-go v0.0.0

require (
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mattn/go-isatty v0.0.19 // indirect
	github.com/rs/zerolog v1.33.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
)

replace github.com/Unluckyathecking/crucible/workers/sdk-go => ../../sdk-go
