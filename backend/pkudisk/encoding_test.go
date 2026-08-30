package pkudisk

import (
	"strings"
	"testing"

	"github.com/rclone/rclone/lib/encoder"
)

func TestDefaultEncodingRoundTripsAnyShareRestrictedNames(t *testing.T) {
	f := &Fs{opt: Options{Enc: defaultEncoding}}
	for _, name := range []string{
		` leading space`,
		`trailing space `,
		`trailing period.`,
		`colon:question?asterisk*quote"lt<gt>pipe|backslash\`,
		"control\x01name",
		`．`, // literal fullwidth replacement characters must remain reversible too
	} {
		standard := encoder.Standard.Encode(name)
		encoded := f.encodeName(standard)
		if got := f.decodeName(encoded); got != standard {
			t.Fatalf("round trip %q -> standard %q -> encoded %q -> %q", name, standard, encoded, got)
		}
		if strings.HasPrefix(encoded, " ") || strings.HasSuffix(encoded, " ") || strings.HasSuffix(encoded, ".") {
			t.Fatalf("encoded name still has AnyShare-normalized edge characters: %q", encoded)
		}
		if strings.ContainsAny(encoded, `\\/:*?"<>|`) {
			t.Fatalf("encoded name still contains AnyShare-forbidden characters: %q", encoded)
		}
	}
}
