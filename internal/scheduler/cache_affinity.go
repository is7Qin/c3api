// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
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
	h := uint64(14695981039346656037)
	for i := 0; i < len(key); i++ {
		h ^= uint64(key[i])
		h *= 1099511628211
	}
	return h
}

func buildCacheDomainRing(domains []string) (CacheDomainRing, error) {
	unique := make(map[string]struct{}, len(domains))
	for _, domain := range domains {
		if domain != "" {
			unique[domain] = struct{}{}
		}
	}
	if len(unique) > MaxCacheDomainRingNodes/CacheDomainVirtualNodes {
		return CacheDomainRing{}, ErrCacheDomainRingCapExceeded
	}

	ordered := make([]string, 0, len(unique))
	for domain := range unique {
		ordered = append(ordered, domain)
	}
	sort.Strings(ordered)
	nodes := make([]CacheDomainNode, 0, len(ordered)*CacheDomainVirtualNodes)
	for _, domain := range ordered {
		for vnode := 0; vnode < CacheDomainVirtualNodes; vnode++ {
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

func cacheDomainNodeHash(domain string, vnode int) uint64 {
	h := CacheAffinityHash(domain)
	h *= 1099511628211
	for shift := uint(56); ; shift -= 8 {
		h ^= uint64(byte(uint64(vnode) >> shift))
		h *= 1099511628211
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
