# Sending files

A file of any size goes from one contact to another peer to peer: directly over QUIC when the two
can connect, through a relay circuit when they cannot. Nobody else, the relay included, can read
or alter it. The code is in `echofog-core-oss/internal/services/filexfer`.

## What the receiver learns first

The sender reads the file once and builds a SHA-256 Merkle tree over its 64 KiB chunks (the tree
of RFC 6962: leaves are `SHA-256(0x00 ‖ chunk)`, inner nodes `SHA-256(0x01 ‖ left ‖ right)`; the
root names the content, as the piece root does in BitTorrent v2). It then sends an ordinary chat
message, end-to-end encrypted like every message, that carries:

- the file name and size,
- the Merkle root,
- a transfer ID and a fresh random 32-byte key.

Nothing moves until the receiver accepts. A receiver checks the size against its limit (32 GiB by
default) and the free space on its disk first.

## How the bytes move

The receiver pulls. It asks for the chunks it lacks in *want* frames; the sender answers each with
a *chunk* frame that holds the chunk and its audit path (the sibling hashes up to the root, 32 bytes
per tree level). The receiver checks every chunk against the root the moment it arrives, as in
Bao/iroh verified streaming, so a chunk that does not belong to the file is never written, and no
piece list is needed up front.

Every frame is sealed with XChaCha20-Poly1305. The two directions use different keys derived from
the offer's key, and the frame header (kind, transfer ID) is authenticated, so a relay can neither
read, alter, inject nor reflect frames. A frame that does not open is dropped.

Flow control works like TCP's: the receiver keeps a window of chunks in flight, grows it while
chunks come back promptly, halves it when one is lost, and asks again for what does not arrive in
time. The sender sends a few frames at once and adapts that number to how fast they leave.

## Interruptions

- **Disconnect.** A transfer that hears nothing for a while shows as *waiting for the contact*, tries
  to reconnect (DHT, hole punch, relay) and goes on by itself when the peer is back. It never gives
  up on its own.
- **App restart.** The receiver writes chunks into `<name>.<id>.part` and records which ones it has,
  only after they were flushed to disk. After a restart it continues from there. The sender serves
  the file again once it checked that the file still has the offered root.
- **Damage.** When the whole file is there it is hashed once more. If it does not match (the
  partial file was changed while the app was closed) it is fetched again.
- **Cancel / decline.** Either side can stop a transfer; the partial file is deleted and the other
  side is told.

## On the receiver's disk

Files land in `Downloads/EchoFog` (or `storage.download_dir`). The name the sender gave is reduced
to a plain file name: no folders, no `..`, no characters or device names Windows refuses, no
invisible direction marks; an existing file is never overwritten (`name (1).ext`). On Windows a
received file carries the Mark of the Web, so the system warns before it runs a program from it,
and the app asks before opening a file that would run code.
