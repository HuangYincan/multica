package daemon

import (
	"strings"
	"testing"
)

func TestFormatTaskSupplementInstructionPreservesOriginalGoal(t *testing.T) {
	got := formatTaskSupplementInstruction("  Ada   Lovelace ", "also include a rollback note")
	for _, want := range []string{
		"additional guidance for the same active task",
		"Preserve and complete the original objective",
		"single final response",
		"only if the human explicitly asks",
		`Human "Ada Lovelace"`,
		"also include a rollback note",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("instruction missing %q:\n%s", want, got)
		}
	}
}
