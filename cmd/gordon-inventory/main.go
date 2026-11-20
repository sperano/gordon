// Command gordon-inventory prints, as the Markdown kept on the shared
// wiki, the legacy dispatch inventory, the Hollingsworth paths of each
// later stage, the Vikunja families' ownership switches and the rollback
// contract.
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
