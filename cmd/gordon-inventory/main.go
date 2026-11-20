// Command gordon-inventory prints the legacy dispatch inventory and the
// Hollingsworth paths of each later stage as the Markdown tables kept on
// the shared wiki.
package main

import (
	"fmt"

	"github.com/sperano/gordon/internal/inventory"
)

func main() {
	fmt.Print(inventory.Markdown(inventory.Entries()))
	fmt.Print("\n")
	fmt.Print(inventory.StagesMarkdown(inventory.Stages()))
	fmt.Print("\n")
	fmt.Print(inventory.SwitchesMarkdown(inventory.Switches()))
	fmt.Print("\n" + inventory.RollbackContract + "\n")
}
