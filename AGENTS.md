# Agent rules

* Develop and test only against the development stack ("Run it locally" in `README.md`). Never use production services or the real library.
* Never print secret values. List Infisical secrets by name only. In zsh, never write `2>&1 >/dev/null` around a pipe: MULTIOS sends output to both.
* Keep real release names out of this repository. They belong in the private `garner-research` repository.
* Before every commit, run `coderabbit review --agent --base-commit <parent> --include-untracked`. Push only after the maintainer confirms.
* Before every push, run `mise run check`. `mise run release` releases only what is pushed and checks nothing itself.
* To run the real-case tests, set `GARNER_MATCH_CASES`, `GARNER_SELECT_CASES` and `GARNER_RELEASE_CASES` to the files in `garner-research/tests/`, then run `go test ./...`.
