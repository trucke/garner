package selection

import (
	"fmt"

	"github.com/trucke/garner/internal/profile"
)

// TV and Anime are the shipped profiles "German" and "German, English
// fallback": the rules of the tv and anime profiles before profiles moved
// into the database. Tests judge releases by them.
var (
	TV    = shipped("German")
	Anime = shipped("German, English fallback")
)

func shipped(name string) Profile {
	for _, p := range profile.Shipped() {
		if p.Name == name {
			return FromProfile(p, "")
		}
	}
	panic("no shipped profile " + name)
}

// fromText reads a profile's text, as an import does, and applies it to a
// show whose original language is original.
func fromText(text string, original string) Profile {
	p, _, err := profile.Parse("garner_profile = 1\nname = \"Test\"\n" + text)
	if err != nil {
		panic(fmt.Sprintf("test profile: %v", err))
	}
	return FromProfile(p, OriginalLanguage(original))
}
