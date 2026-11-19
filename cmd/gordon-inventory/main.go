// Command gordon-inventory prints the legacy dispatch inventory as the
// Markdown table kept on the shared wiki.
package main

import (
	"fmt"

	"github.com/sperano/gordon/internal/inventory"
)

func main() { fmt.Print(inventory.Markdown(inventory.Entries())) }
