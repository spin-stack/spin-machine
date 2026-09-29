module github.com/spin-stack/spin-machine

go 1.27

tool (
	github.com/spin-stack/go-tools/cmd/ctxlife
	github.com/spin-stack/go-tools/cmd/mutate
	github.com/spin-stack/go-tools/cmd/refs
	github.com/spin-stack/go-tools/cmd/testquality
	github.com/spin-stack/go-tools/cmd/versions
)

require github.com/spin-stack/go-tools v0.0.0-20260929100035-f55da7c4ecef

require (
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/tools v0.50.0 // indirect
)
