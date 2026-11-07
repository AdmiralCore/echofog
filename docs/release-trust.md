Русская версия: [release-trust.ru.md](release-trust.ru.md)

# Release trust: the key in install.sh, min_version, key rotation

This page is for the person who publishes releases and for operators who want to know what exactly the
installation path trusts. The step-by-step server guide is [deploy-vps-v2.md](deploy-vps-v2.md).

## 1. What the trust root is

The root of trust of a server installation is the **release public key block inside `install.sh`**
(between the lines `# BEGIN RELEASE KEYS` and `# END RELEASE KEYS`): three Ed25519 keys and a key epoch
number. They are the same release keys that the application checks when it updates itself (two of the
three must sign).

- The script downloads `release.txt` (a short text record: version, `min_version`, key epoch, and the
  size and SHA-256 of every file; the epoch is the `key_epoch` line) and `release.txt.sigs` (armored signatures).
- It checks them with `ssh-keygen -Y verify`, a tool of the base system (OpenSSH 8.0 or newer). No other
  tool is trusted for this and nothing downloaded is run for it. If `ssh-keygen` is missing or too old,
  the script stops before anything is downloaded or run.
- Only then does it fetch the binary, compare size and SHA-256 with the signed record, make the file
  executable and start it. On any mismatch the file is deleted and the script stops.
- The node's own installer (`echofog-node v2 install`) checks the binary once more against the signed
  manifest. It is a second layer, no longer the first check.

The fingerprint of the key block is the SHA-256 of the exact text between the two marker lines
(markers included). An operator gets it in two ways that must agree:

```
sh install.sh --fingerprint
sed -n '/^# BEGIN RELEASE KEYS$/,/^# END RELEASE KEYS$/p' install.sh | sha256sum
```

The fingerprint covers the keys and the key epoch, and nothing else, so it stays the same from release
to release until the keys change. The version floor of the script is outside it on purpose.

## 2. Text for the website page

The fingerprint is published in two independent places, the website and the repository README. The
release owner fills in the value that `efv2-release install-stamp` printed. This text is for the site
page:

> **Check install.sh before running it.** Download the file (`curl --proto '=https' --tlsv1.2 -fsSL -o
> install.sh <address>`), then run `sh install.sh --fingerprint`. The value must be exactly:
> `<FINGERPRINT>`. The same value is published in the README of the repository on GitHub; if the two
> places differ, do not run the script and tell the project. Never pipe the download into a shell.

## 3. Text for the repository README

> **Release key fingerprint.** Installation of a server node uses `install.sh`, which carries the
> release public keys. Its key block has the SHA-256 fingerprint `<FINGERPRINT>` (key epoch
> `<EPOCH>`). Compare it with the value on the project website before the first run: `sh install.sh
> --fingerprint`. See [docs/release-trust.md](release-trust.md) for what is verified and how the keys
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
stamped and prints the fingerprint. Check the stamped script once with `sh out/install.sh --fingerprint`
and put the value on the website and in the README **before** announcing the release.

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
   its fingerprint on the website and in the README, then publish the first release signed by the new
   keys with `install-record ... --key-epoch 2`.

What the scripts do: an **old** script (epoch 1) that finds a valid rotation record stops, prints the
fingerprint of the new key block and asks for the new `install.sh`; a rotation record signed by anybody
else is ignored. A **new** script (epoch 2) refuses every release of epoch 1, so it exits before the
first release under the new keys exists. A script never switches keys by itself.

If a release key is stolen, the rotation record signed by the old keys cannot be trusted: announce the
new fingerprint on the website and in the README through the channels that were not compromised, and
tell operators to replace `install.sh` by hand.

## 7. What this does not cover

- Someone who controls both the place where `install.sh` is downloaded and the place where the
  fingerprint is published (two independent places are the defence).
- A compromised build machine or release key before the owner notices; rotation limits the damage.
- A system with a tampered `ssh-keygen` or `sha256sum`: they are part of the base system and trusted
  as such.
- Run on a clean Debian or Ubuntu system, the tests in `deploy/v2` start the real script with test keys;
  a real server run with the real keys is done by the owner at the first release.
