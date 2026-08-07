package cli

import "fmt"

// plural writes a count with its singular or plural noun for command summaries.
func plural(count int, noun string) string {
	if count == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", count, noun)
}
