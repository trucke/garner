# Agent rules

Follow [CONTRIBUTING.md](CONTRIBUTING.md); its ground rules apply to agents too. In addition:

* Develop and test only against the development stack ("Run it locally" in `CONTRIBUTING.md`). Never use other services or a real library.
* Never print secret values. List secrets by name only. In zsh, never write `2>&1 >/dev/null` around a pipe: MULTIOS sends output to both.
* Keep real release names out of this repository, including tests, comments and commit messages.
* Before every commit, run `coderabbit review --agent --base-commit <parent> --include-untracked` when CodeRabbit is available. Push only after the user confirms.
* Before every push, run `mise run check`. `mise run release` releases only what is pushed and checks nothing itself.
* To include the private real-case tests, set `GARNER_MATCH_CASES`, `GARNER_SELECT_CASES` and `GARNER_RELEASE_CASES` to the case files in the maintainer's private `garner-research` repository (`tests/`), then run `go test ./...`.
