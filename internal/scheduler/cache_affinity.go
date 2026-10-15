// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"encoding/binary"
	"errors"
	"sort"
	"strconv"
)

const (
	CacheDomainVirtualNodes = 32
	MaxCacheDomainRingNodes = 1 << 20
)

var ErrCacheDomainRingCapExceeded = errors.New("scheduler: cache-domain ring node cap exceeded")

type CacheDomainNode struct {
	Hash   uint64
	Domain string
}

type CacheDomainRing struct {
	Nodes   []CacheDomainNode
	Domains []string
}

type CacheDomainAccount struct {
	AccountID int64
	Domain    string
}

func CacheAffinityHash(key string) uint64 {
	h := uint64(fnvOffset64)
	for i := 0; i < len(key); i++ {
		h ^= uint64(key[i])
		h *= fnvPrime64
	}
	return h
}

// cacheDomainUnique returns the distinct non-empty domains (unsorted), so the
// cap can be checked before paying the sort on an oversized input.
func cacheDomainUnique(domains []string) map[string]struct{} {
	unique := make(map[string]struct{}, len(domains))
	for _, domain := range domains {
		if domain != "" {
			unique[domain] = struct{}{}
		}
	}
	return unique
}

func orderedDomains(unique map[string]struct{}) []string {
	ordered := make([]string, 0, len(unique))
	for domain := range unique {
		ordered = append(ordered, domain)
	}
	sort.Strings(ordered)
	return ordered
}

// normalizeCacheDomains returns the exact domain-set identity: non-empty,
// deduplicated, lexicographically sorted. Sharing this one normalization keeps
// a ring and its intern key from ever drifting apart.
func normalizeCacheDomains(domains []string) []string {
	return orderedDomains(cacheDomainUnique(domains))
}

func buildCacheDomainRing(domains []string) (CacheDomainRing, error) {
	return buildCacheDomainRingOrdered(normalizeCacheDomains(domains))
}

// buildCacheDomainRingOrdered builds a ring from an already-normalized domain
// set (deduplicated + sorted), skipping the second normalization pass.
func buildCacheDomainRingOrdered(ordered []string) (CacheDomainRing, error) {
	if len(ordered) > MaxCacheDomainRingNodes/CacheDomainVirtualNodes {
		return CacheDomainRing{}, ErrCacheDomainRingCapExceeded
	}
	nodes := make([]CacheDomainNode, 0, len(ordered)*CacheDomainVirtualNodes)
	for _, domain := range ordered {
		for vnode := range CacheDomainVirtualNodes {
			nodes = append(nodes, CacheDomainNode{Hash: cacheDomainNodeHash(domain, vnode), Domain: domain})
		}
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Hash != nodes[j].Hash {
			return nodes[i].Hash < nodes[j].Hash
		}
		return nodes[i].Domain < nodes[j].Domain
	})
	return CacheDomainRing{Nodes: nodes, Domains: ordered}, nil
}

// cacheDomainSetKey encodes a normalized domain set as an unambiguous
// length-prefixed key: uvarint(len(domain)) || domain, in sorted order. The
// length prefix removes delimiter ambiguity ({"a","bc"} keys differently from
// {"ab","c"}), so the key is the exact set identity — never a non-collision-
// checked hash.
func cacheDomainSetKey(ordered []string) string {
	if len(ordered) == 0 {
		return ""
	}
	var b []byte
	var tmp [binary.MaxVarintLen64]byte
	for _, domain := range ordered {
		n := binary.PutUvarint(tmp[:], uint64(len(domain)))
		b = append(b, tmp[:n]...)
		b = append(b, domain...)
	}
	return string(b)
}

// ringInterner is a per-compile session sharing immutable cache-domain rings
// across routes with the same actual-domain-set key. Owner: the compile lane
// (one session per full Compile, one per scoped fire). It is never global and
// never stored on the stateless RoutingCompiler; the map (including any
// temporarily indexed soon-to-be-deleted ring) dies with the session. Indexed
// rings are never mutated — only slice headers enter the map.
type ringInterner struct {
	rings map[string]CacheDomainRing
}

func newRingInterner() *ringInterner {
	return &ringInterner{rings: make(map[string]CacheDomainRing)}
}

// intern returns the shared ring for the actual domain set, building and
// registering it on a miss. The distinct count is cap-checked before the sort,
// normalization runs once, and a cap error propagates unchanged (never
// registered). A nil session degrades to independent construction (the
// single-route test seam).
func (s *ringInterner) intern(domains []string) (CacheDomainRing, error) {
	if s == nil {
		return buildCacheDomainRing(domains)
	}
	unique := cacheDomainUnique(domains)
	if len(unique) > MaxCacheDomainRingNodes/CacheDomainVirtualNodes {
		return CacheDomainRing{}, ErrCacheDomainRingCapExceeded
	}
	ordered := orderedDomains(unique)
	key := cacheDomainSetKey(ordered)
	if ring, ok := s.rings[key]; ok {
		return ring, nil
	}
	ring, err := buildCacheDomainRingOrdered(ordered)
	if err != nil {
		return CacheDomainRing{}, err
	}
	s.rings[key] = ring
	return ring, nil
}

// seedFromView indexes every already-published ring in dv under its actual
// domain-set key, so a scoped fire reuses surviving carry rings instead of
// rebuilding them. Published rings are immutable: only their slice headers are
// copied into the session map. The shared Nodes backing is deduplicated by its
// head pointer first, so the canonical key is built once per UNIQUE ring
// (O(U×D), not O(R)) without unsafe. Nil-safe.
func (s *ringInterner) seedFromView(dv *DecisionView) {
	if s == nil || dv == nil {
		return
	}
	seen := make(map[*CacheDomainNode]struct{})
	for _, rd := range dv.routes {
		if rd == nil || len(rd.CacheDomainRing.Nodes) == 0 {
			continue
		}
		head := &rd.CacheDomainRing.Nodes[0]
		if _, ok := seen[head]; ok {
			continue
		}
		seen[head] = struct{}{}
		key := cacheDomainSetKey(rd.CacheDomainRing.Domains)
		if _, ok := s.rings[key]; !ok {
			s.rings[key] = rd.CacheDomainRing
		}
	}
}

func cacheDomainNodeHash(domain string, vnode int) uint64 {
	h := CacheAffinityHash(domain)
	h *= fnvPrime64
	for shift := uint(56); ; shift -= 8 {
		h ^= uint64(byte(uint64(vnode) >> shift))
		h *= fnvPrime64
		if shift == 0 {
			break
		}
	}
	return h
}

func privateCacheDomain(accountID int64) string {
	return "private:" + strconv.FormatInt(accountID, 10)
}

func (r CacheDomainRing) Lookup(hash uint64) (string, bool) {
	if len(r.Nodes) == 0 {
		return "", false
	}
	index := sort.Search(len(r.Nodes), func(i int) bool { return r.Nodes[i].Hash >= hash })
	if index == len(r.Nodes) {
		index = 0
	}
	return r.Nodes[index].Domain, true
}

func cacheDomainForAccount(accountID int64, domain *string) string {
	if domain != nil && *domain != "" {
		return *domain
	}
	return privateCacheDomain(accountID)
}

func cacheDomainAccountDomain(accounts []CacheDomainAccount, accountID int64) (string, bool) {
	index := sort.Search(len(accounts), func(i int) bool { return accounts[i].AccountID >= accountID })
	if index == len(accounts) || accounts[index].AccountID != accountID {
		return "", false
	}
	return accounts[index].Domain, true
}
