Русская версия: [release-trust.ru.md](release-trust.ru.md)

# Release trust: the key in install.sh, min_version, key rotation

This page is for the person who publishes releases and for operators who want to know what exactly the
installation path trusts. The step-by-step server guide is [deploy-vps-v2.md](deploy-vps-v2.md).

## 1. What the trust root is

The root of trust of a server installation is the **release public key block inside `install.sh`**
(between the lines `# BEGIN RELEASE KEYS` and `# END RELEASE KEYS`): three Ed25519 keys and a key epoch
number. They are the same release keys that the application checks when it updates itself (at least
two of them must sign).

- The script downloads `release.txt` (a short text record: version, `min_version`, key epoch, and the
  size and SHA-256 of every file; the epoch is the `key_epoch` line) and `release.txt.sigs` (armored signatures).
- It checks them with `ssh-keygen -Y verify`, a tool of the base system (OpenSSH 8.0 or newer). No other
  tool is trusted for this and nothing downloaded is run for it. If `ssh-keygen` is missing or too old,
  the script stops before anything is downloaded or run.
- Only then does it fetch the binary, compare size and SHA-256 with the signed record, make the file
  executable and start it. On any mismatch the file is deleted and the script stops.
- The node's own installer (`echofog-node v2 setup`) checks the binary once more against the signed
  manifest. It is a second layer, no longer the first check.

What users compare is the SHA-256 of the **whole `install.sh`**, not of the key block alone: the key block
says which keys are trusted, but the rest of the file is the logic that uses them (the checks, their order,
the version floor), and a changed script with the same key block must not pass. The command runs no code
from the file:

```
sha256sum install.sh
```

The value changes with every release, because `install-stamp` writes the version floor of the release
into the file. It is published for each release in two independent places before the announcement.
`sh install.sh --fingerprint` prints the same value and, on a second line, the fingerprint of the key
block alone; it is **not** a proof, because a changed script can print anything.

The key block has its own value, the SHA-256 of the exact text between the two marker lines (markers
included). It covers the keys and the key epoch, stays the same until the keys change, and is what the
key rotation record names (section 6). It is printed by `install-stamp`, `install-fingerprint --keys ...
--key-epoch N` and, on the `keys` line, by `sh install.sh --fingerprint`.

## 2. Text for the website page

The value is published in two independent places, the website and the repository README. The
release owner fills in the value that `efv2-release install-stamp` printed. This text is for the site
page:

> **Check install.sh before running it.** Download the file (`curl --proto '=https' --tlsv1.2 -fsSL -o
> install.sh <address>`), then run `sha256sum install.sh`. The value must be exactly:
> `<FINGERPRINT>`. The same value is published in the README of the repository on GitHub; if the two
> places differ, do not run the script and tell the project. Never pipe the download into a shell.

## 3. Text for the repository README

> **install.sh fingerprint.** Installation of a server node uses `install.sh`, which carries the
> release public keys. For release `<VERSION>` the SHA-256 of the whole file is `<FINGERPRINT>` (key
> epoch `<EPOCH>`). Compare it with the value on the project website before the first run:
> `sha256sum install.sh`. See [docs/release-trust.md](release-trust.md) for what is verified and how the keys
> are replaced.

## 4. min_version and rollback

`min_version` is a field of the signed release manifest (field 8 of `Release`, an additive change: nodes
that do not know it ignore it, and it is covered by the signatures). It is the oldest version that may
be installed or kept. It is copied into `release.txt` as `min_version`.

- The script and `echofog-node v2 update` refuse a version **below `min_version`**, and refuse a version
  **below the installed one**.
- A rollback above `min_version` needs the explicit flag (`--allow-rollback` for both) and prints a
  warning. The flag never goes below `min_version`, and the script never goes below the version floor
  that was stamped into it at release time.
- A manifest whose `min_version` is above its own version, or is not a plain `X.Y.Z`, is not a manifest.

Raise `min_version` when an older version has a fixed hole that must not come back. It is a one-way
tool: it cannot be lowered by an older manifest, because an older manifest is not accepted at all.

## 5. Making a release

All steps run on the offline machine of the release owner. The keys are made once with
`efv2-release keygen` on offline media; they and the real fingerprint never go into the repository.
Tests use test keys.

```
efv2-release assemble --version 2.0.1 --min-version 2.0.0 --released-at <UNIX> --artifact ... --out manifest.bin
efv2-release cosign   --manifest manifest.bin --key K2.key --out K2.cosig
efv2-release sign     --manifest manifest.bin --key K1.key --cosig K2.cosig --not-after <UNIX> --out release.bin
efv2-release install-record --manifest manifest.bin --key K1.key --key K2.key --key-epoch 1 --out-dir out/
efv2-release install-stamp  --template deploy/v2/install.sh --keys PUB1,PUB2,PUB3 --key-epoch 1 --floor 2.0.0 --out out/install.sh
```

Publish in the release folder: `release.bin` as `manifest.bin`, `release.txt`, `release.txt.sigs` and
the binaries `echofog-node-linux-amd64` and `echofog-node-linux-arm64`; publish `install.sh` beside them.
`install-record` refuses fewer than two different keys; `install-stamp` refuses a script that was already
stamped and prints two values: `file` (the SHA-256 of the whole script) and `keys` (the key block). Check
the stamped script once with `sha256sum out/install.sh` and put the `file` value on the website and in
the README **before** announcing the release. A script stamped again for another release has another
value.

### The same keys in the binary

`install.sh` carries the keys as text; the binary carries the same public keys compiled in. Both lists
must match, otherwise the script accepts a release that the node then refuses (or the reverse). The
build receives them as a comma-separated list in `RELEASE_KEYS`:

```
make build VERSION=0.3.0 RELEASE_KEYS='<KEY1>,<KEY2>,<KEY3>'
```

In CI the same value comes from the repository secret `RELEASE_KEYS`; without it the build is an OSS
build. Each item is an Ed25519 public key in base64 or hex; the `ed25519:<base64url>` text form of
`efv2-release` is accepted too. Keys are public. Private keys never leave the offline machine.

An OSS build compiles an empty set: `echofog-node v2 setup` and `v2 update` answer "this build has no
release keys", and no signed entry list is trusted. That is the intended fail-closed behaviour. A
non-empty but malformed list does not silently become empty: the binary panics at startup with the
offending key number, so a broken release build cannot ship.

For 0.3.0 the built-in entry list is signed by one release key; the update manifest already requires
at least two distinct release-key signatures. Moving the entry list to a 2-of-3 signature is planned
after 0.3.0.

## 6. Replacing the release keys

The old keys vouch for the new ones, so that nobody has to trust a new key blindly:

1. Make the new key set offline (`efv2-release keygen` three times).
2. Publish a release whose manifest carries the new keys as `--next-key` (the application's own key-set
   transition, objects §9).
3. Write the rotation record. It is signed by two **old** keys and names the fingerprint of the new key
   block and the new epoch:

   ```
   efv2-release install-rotation --new-keys NEW1,NEW2,NEW3 --new-epoch 2 --key OLD1.key --key OLD2.key --out-dir out/
   ```

   Publish `rotation.txt` and `rotation.txt.sigs` in the release folder and keep them there as long as
   old copies of `install.sh` are in use.
4. Stamp the new script with the new keys and the new epoch (`install-stamp ... --key-epoch 2`), publish
   its `file` value on the website and in the README, then publish the first release signed by the new
   keys with `install-record ... --key-epoch 2`.

What the scripts do: an **old** script (epoch 1) that finds a valid rotation record stops, prints the
fingerprint of the new key block and asks for the new `install.sh`; a rotation record signed by anybody
else is ignored. A **new** script (epoch 2) refuses every release of epoch 1, so it exits before the
first release under the new keys exists. A script never switches keys by itself.

If a release key is stolen, the rotation record signed by the old keys cannot be trusted: announce the
new fingerprint on the website and in the README through the channels that were not compromised, and
tell operators to replace `install.sh` by hand.

## 7. What this does not cover

- Someone who controls both the place where `install.sh` is downloaded and the places where the
  value is published (two independent places are the defence).
- A compromised build machine or release key before the owner notices; rotation limits the damage.
- A system with a tampered `ssh-keygen` or `sha256sum`: they are part of the base system and trusted
  as such.
- Run on a clean Debian or Ubuntu system, the tests in `deploy/v2` start the real script with test keys;
  a real server run with the real keys is done by the owner at the first release.
