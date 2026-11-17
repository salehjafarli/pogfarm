// Command pogfarm is the main service: it runs once, on the main node.
package main

import "fmt"

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	fmt.Printf("pogfarm %s\n", version)
}
