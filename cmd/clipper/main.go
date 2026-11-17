// Command clipper is the render service: it runs on every node.
package main

import "fmt"

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	fmt.Printf("clipper %s\n", version)
}
