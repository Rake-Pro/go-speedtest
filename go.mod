module github.com/Rake-Pro/go-speedtest

go 1.27.3

// No Multipath TCP listeners: nothing on the fleet dials MPTCP, and a listening
// MPTCP socket was the precondition of the CVE-2026-98432 host-kernel exploit.
godebug multipathtcp=0

require (
	github.com/rs/zerolog v1.35.1
	golang.org/x/net v0.62.0
	modernc.org/sqlite v1.61.1
)

require (
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	modernc.org/libc v1.79.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
