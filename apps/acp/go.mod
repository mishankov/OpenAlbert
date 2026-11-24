module openalbert/apps/acp

go 1.26.0

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/google/uuid v1.6.0
	github.com/mishankov/OpenAlbert/shared/codemode v0.0.0
)

require (
	github.com/tetratelabs/wazero v1.12.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
)

replace github.com/mishankov/OpenAlbert/shared/codemode => ../../shared/codemode
