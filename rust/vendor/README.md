# Vendored Liquid core

`liquid-core/` comes from the crates.io release `liquid-core` 0.26.11:

- Archive: <https://static.crates.io/crates/liquid-core/liquid-core-0.26.11.crate>
- SHA-256: `fc623edee8a618b4543e8e8505584f4847a4e51b805db1af6d9af0a3395d0d57`
- License: MIT or Apache-2.0; both license files are included.

The archive checksum was verified against the original workspace lockfile.
The only source change is to the normalized `Cargo.toml` and its
`Cargo.toml.orig`: the `anymap2` dependency now names package `anymap3`
version `1.1`, retaining the `anymap2` import alias. No Rust source files
are modified. The published library's standalone generated `Cargo.lock`
is omitted; `rust/Cargo.lock` governs all dependencies used by Symphony.

Liquid 0.26.11 still depends on the unmaintained `anymap2` crate, and
upstream has not migrated it. This local patch addresses
[RUSTSEC-2026-0319](https://rustsec.org/advisories/RUSTSEC-2026-0319.html)
using its recommended replacement without changing prompt semantics.

When upstream releases a core using the maintained dependency, update
Liquid, remove the `liquid-core` entry from `[patch.crates-io]`, and remove
this directory. Until then, compare any update against its checksum-verified
release archive and reapply only the dependency substitution. Run the
Rust format, Clippy, workspace test, and strict supply-chain gates after
each update.
