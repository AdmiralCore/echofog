// Package store is the L6 storage layer of protocol v2.
//
// A store is one directory per profile, <data>/v2 (AD7, 02-architecture §2), laid out by Path.
// Every store starts with its schema number (versioning.md §3.7): a v2 build refuses a store with
// a higher number and migrates a lower one forward after writing a copy of it. Protocol v2 starts
// at schema 200.
//
// Values are encrypted with the account's storage key (T4.1) before they reach badger; a value is
// bound to its namespaced key as associated data, so it cannot be moved under another key. Keys
// stay in the clear so that ordering and prefix scans work; a service blinds identifiers before
// it builds them when they identify people.
//
// Services do not see the database. They get one namespace at a time through Store(ns), which is
// the KV that node.Host.Store(ns) returns (02-architecture §3.1). Namespaces are separated by an
// encoded prefix, so "a" with key "b:c" and "a:b" with key "c" are different records.
//
// Under a positive MemCapMB the badger options follow platforms.md §1.2 for phones: 4 MiB
// memtables, at most 8 MiB of block cache, at most 4 MiB of index cache, a 16 MiB value log and
// the smallest number of compaction workers badger supports.
//
// Badger dies on open when a crash leaves an unwritten or half-written table (the v1 incidents of
// 30 October). Open repairs such tables first, using the v1 repair tools with the table size this
// store was written with, and keeps a copy of the database before changing it.
package store
