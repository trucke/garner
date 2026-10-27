package api

// Profile is a stored profile. Text holds it in the format users share
// (docs/profiles.md); clients read it with package profile, so the fields
// have one definition.
type Profile struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Text string `json:"text"`
	// DefaultTV and DefaultAnime mark the profile new shows of a type get.
	DefaultTV    bool `json:"default_tv"`
	DefaultAnime bool `json:"default_anime"`
	// Shows names the shows that use the profile.
	Shows []string `json:"shows"`
}
