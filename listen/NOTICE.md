# Listen Together party server

This package is the Listen Together backend from [BitChord](https://github.com/kushagrasinghx/BitChord)
(`backend/`, as of upstream `acf219a7`, BitChord v1.10), by Kushagra Singh and the BitChord
contributors, licensed under the GNU General Public License v3.0 like this project.

Changes for the companion: `package main` became `package listen`, `main()` became `Handler()`
(the party routes, mounted by the companion, without the server's own root page) and `Owns()`,
and import paths were moved into this module. The protocol, party logic and tests are unchanged.
