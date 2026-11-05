package dht

import (
	"context"
	"encoding/hex"
	"sort"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/objects"
)

// A lookup is one iterative search for a key over one or more paths (dht.md §4). Each path is a
// Kademlia lookup that continues only from the nodes its own path returned. Paths share no node
// and no diversity unit: a node (or any unit of its addresses) is claimed by the first path that
// queries it, and the other paths skip it for good (S/Kademlia disjoint paths).

type candState uint8

const (
	candNew candState = iota
	candInFlight
	candAnswered
	candFailed
)

type lcand struct {
	card  *objects.Card
	id    DeviceID
	units []string
	wide  []string
	state candState
	// record reports that the node answered with a valid record. A node that answered without one
	// said "not found".
	record bool
}

type lpath struct {
	lk      *lookup
	idx     int
	cands   map[DeviceID]*lcand
	queries int
}

type lookup struct {
	c      *Client
	key    [32]byte
	value  bool
	accept func([]byte) (*objects.Envelope, error)

	mu        sync.Mutex
	paths     []*lpath
	owner     map[DeviceID]int
	unitOwner map[string]int
	best      *objects.Envelope
}

func (c *Client) newLookup(key [32]byte, nPaths int, value bool, accept func([]byte) (*objects.Envelope, error)) *lookup {
	lk := &lookup{
		c: c, key: key, value: value, accept: accept,
		owner:     map[DeviceID]int{},
		unitOwner: map[string]int{},
	}
	for i := 0; i < nPaths; i++ {
		lk.paths = append(lk.paths, &lpath{lk: lk, idx: i, cands: map[DeviceID]*lcand{}})
	}
	// Starting nodes: the closest the table knows, dealt to the paths in distance order. Nodes of
	// one diversity unit go to the same path, so the paths start disjoint.
	seedUnit := map[string]int{}
	next := 0
	for _, card := range c.cfg.Table.Closest(key, 3*K*nPaths) {
		if card == nil || DeviceID(card.DeviceID) == c.cfg.Self || !card.Publishable() {
			continue
		}
		units, _ := unitsOf(card)
		idx := -1
		for _, u := range units {
			if p, ok := seedUnit[u]; ok {
				idx = p
				break
			}
		}
		if idx < 0 {
			idx = next % nPaths
			next++
		}
		for _, u := range units {
			seedUnit[u] = idx
		}
		lk.paths[idx].add(card)
	}
	return lk
}

// unitsOf returns the diversity units (IPv4 /24, IPv6 /32) and the wide units (/16, /24) of a
// card's addresses. A card without an IP address is a unit of its own (its device).
func unitsOf(card *objects.Card) (units, wide []string) {
	e := newEntry(card, time.Time{})
	for _, u := range e.units {
		units = append(units, u.String())
	}
	for _, w := range e.wide {
		wide = append(wide, w.String())
	}
	if len(units) == 0 {
		units = []string{"dev:" + hex.EncodeToString(card.DeviceID[:])}
	}
	return units, wide
}

func (p *lpath) add(card *objects.Card) {
	id := DeviceID(card.DeviceID)
	if _, ok := p.cands[id]; ok {
		return
	}
	units, wide := unitsOf(card)
	p.cands[id] = &lcand{card: card, id: id, units: units, wide: wide}
}

// eligible reports whether the path may use the candidate: nobody else has claimed the node or any
// of its units. The caller holds lk.mu.
func (p *lpath) eligible(cd *lcand) bool {
	if o, ok := p.lk.owner[cd.id]; ok && o != p.idx {
		return false
	}
	for _, u := range cd.units {
		if o, ok := p.lk.unitOwner[u]; ok && o != p.idx {
			return false
		}
	}
	return true
}

func (p *lpath) claim(cd *lcand) {
	p.lk.owner[cd.id] = p.idx
	for _, u := range cd.units {
		p.lk.unitOwner[u] = p.idx
	}
}

// sorted returns the candidates ordered by distance to the key.
func (p *lpath) sorted(keep func(*lcand) bool) []*lcand {
	list := make([]*lcand, 0, len(p.cands))
	for _, cd := range p.cands {
		if keep(cd) {
			list = append(list, cd)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		return lessDistance(list[i].id, list[j].id, DeviceID(p.lk.key))
	})
	return list
}

// closestSet forms the "20 closest" of dht.md §4.1 from the candidates keep accepts: at most one
// per diversity unit and two per /16 (IPv6 /24).
func closestSet(list []*lcand) []*lcand {
	var out []*lcand
	units := map[string]bool{}
	wide := map[string]int{}
	for _, cd := range list {
		clash := false
		for _, u := range cd.units {
			if units[u] {
				clash = true
			}
		}
		for _, w := range cd.wide {
			if wide[w] >= 2 {
				clash = true
			}
		}
		if clash {
			continue
		}
		for _, u := range cd.units {
			units[u] = true
		}
		for _, w := range cd.wide {
			wide[w]++
		}
		out = append(out, cd)
		if len(out) == K {
			break
		}
	}
	return out
}

// answeredClosest returns the cards of the closest nodes of this path that answered.
func (p *lpath) answeredClosest() []*objects.Card {
	p.lk.mu.Lock()
	defer p.lk.mu.Unlock()
	var out []*objects.Card
	for _, cd := range closestSet(p.sorted(func(cd *lcand) bool { return cd.state == candAnswered })) {
		out = append(out, cd.card)
	}
	return out
}

// verdict counts, among the answering closest nodes, those that said "not found". It reports
// whether a strict majority did and whether anyone answered at all.
func (p *lpath) verdict() (answered int, majorityNo bool) {
	p.lk.mu.Lock()
	defer p.lk.mu.Unlock()
	set := closestSet(p.sorted(func(cd *lcand) bool { return cd.state == candAnswered }))
	no := 0
	for _, cd := range set {
		if !cd.record {
			no++
		}
	}
	return len(set), len(set) > 0 && no*2 > len(set)
}

// notFound reports whether every path concluded "not found" (dht.md §4.2).
func (lk *lookup) notFound() bool {
	if lk.best != nil {
		return false
	}
	for _, p := range lk.paths {
		if _, no := p.verdict(); !no {
			return false
		}
	}
	return true
}

func (lk *lookup) run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, p := range lk.paths {
		wg.Add(1)
		go func(p *lpath) {
			defer wg.Done()
			p.run(ctx)
		}(p)
	}
	wg.Wait()
}

type answer struct {
	cd    *lcand
	value []byte
	cards [][]byte
	err   error
}

func (p *lpath) run(ctx context.Context) {
	lk := p.lk
	for ctx.Err() == nil {
		lk.mu.Lock()
		if p.queries >= MaxQueries {
			lk.mu.Unlock()
			return
		}
		var batch []*lcand
		for _, cd := range closestSet(p.sorted(func(cd *lcand) bool {
			return cd.state != candFailed && p.eligible(cd)
		})) {
			if cd.state != candNew {
				continue
			}
			if p.queries+len(batch) >= MaxQueries || len(batch) == Alpha {
				break
			}
			batch = append(batch, cd)
		}
		if len(batch) == 0 {
			lk.mu.Unlock()
			return
		}
		for _, cd := range batch {
			p.claim(cd)
			cd.state = candInFlight
			p.queries++
		}
		lk.mu.Unlock()

		answers := make([]answer, len(batch))
		var wg sync.WaitGroup
		for i, cd := range batch {
			if lk.c.cfg.OnQuery != nil {
				lk.c.cfg.OnQuery(p.idx, cd.id)
			}
			wg.Add(1)
			go func(i int, cd *lcand) {
				defer wg.Done()
				qctx, cancel := context.WithTimeout(ctx, QueryTimeout)
				defer cancel()
				a := answer{cd: cd}
				if lk.value {
					a.value, a.cards, a.err = lk.c.cfg.Transport.FindValue(qctx, cd.card, lk.key)
				} else {
					a.cards, a.err = lk.c.cfg.Transport.FindNode(qctx, cd.card, lk.key)
				}
				answers[i] = a
			}(i, cd)
		}
		wg.Wait()
		for _, a := range answers {
			p.take(a)
		}
	}
}

// take applies one answer. Every card and the record are verified here; an answer with anything
// invalid in it counts against the node: the node is dropped from the lookup and says nothing
// about the record (dht.md §4.1).
func (p *lpath) take(a answer) {
	lk := p.lk
	c := lk.c
	now := c.now()

	fail := func() {
		lk.mu.Lock()
		a.cd.state = candFailed
		lk.mu.Unlock()
	}
	if a.err != nil {
		fail()
		return
	}
	if len(a.cards) > MaxCardsPerAnswer {
		fail()
		return
	}
	var cards []*objects.Card
	for _, raw := range a.cards {
		card, err := objects.VerifyCard(raw, now)
		if err != nil || !card.Publishable() {
			c.log.Debug("dht: invalid card in an answer", "from", a.cd.id, "err", err)
			fail()
			return
		}
		cards = append(cards, card)
	}
	var env *objects.Envelope
	if lk.value && len(a.value) > 0 {
		var err error
		if len(a.value) > maxRecordBytes {
			err = ErrNotStorable
		} else {
			env, err = lk.accept(a.value)
		}
		if err != nil {
			c.log.Debug("dht: invalid record in an answer", "from", a.cd.id, "err", err)
			fail()
			return
		}
	}

	lk.mu.Lock()
	a.cd.state = candAnswered
	if env != nil {
		a.cd.record = true
		if better(env, lk.best) {
			lk.best = env
		}
	}
	for _, card := range cards {
		if DeviceID(card.DeviceID) != c.cfg.Self {
			p.add(card)
		}
	}
	lk.mu.Unlock()

	for _, card := range cards {
		if DeviceID(card.DeviceID) != c.cfg.Self {
			c.cfg.Learn(card)
		}
	}
}
