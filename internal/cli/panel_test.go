package cli

import "testing"

func TestHideSecretInDiff(t *testing.T) {
	in := "@@ -1,3 +1,3 @@\n mode: rule\n-secret: old\n+secret: \"new\"\n  secret: nested\n"
	want := "@@ -1,3 +1,3 @@\n mode: rule\n-secret: (hidden)\n+secret: (hidden)\n  secret: nested\n"
	if got := hideSecretInDiff(in); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}
