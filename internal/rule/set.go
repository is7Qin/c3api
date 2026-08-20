// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package rule

import "strings"

const smallSetThreshold = 4

type intSet struct {
	vals []int
	m    map[int]struct{}
}

func newIntSet(in []int) (*intSet, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) <= smallSetThreshold {
		vals := make([]int, len(in))
		copy(vals, in)
		return &intSet{vals: vals}, nil
	}
	m := make(map[int]struct{}, len(in))
	for _, v := range in {
		m[v] = struct{}{}
	}
	return &intSet{m: m}, nil
}

func (s *intSet) contains(v *int) bool {
	if s == nil || v == nil {
		return false
	}
	if s.m != nil {
		_, ok := s.m[*v]
		return ok
	}
	for _, x := range s.vals {
		if x == *v {
			return true
		}
	}
	return false
}

type stringSet struct {
	vals []string
	m    map[string]struct{}
}

func newStringSet(in []string) (*stringSet, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) <= smallSetThreshold {
		vals := make([]string, len(in))
		copy(vals, in)
		return &stringSet{vals: vals}, nil
	}
	m := make(map[string]struct{}, len(in))
	for _, v := range in {
		m[v] = struct{}{}
	}
	return &stringSet{m: m}, nil
}

func (s *stringSet) contains(v string) bool {
	if s == nil {
		return false
	}
	if s.m != nil {
		_, ok := s.m[v]
		return ok
	}
	for _, x := range s.vals {
		if x == v {
			return true
		}
	}
	return false
}

type substringSet struct {
	vals []string
}

func newSubstringSet(in []string) (*substringSet, error) {
	if len(in) == 0 {
		return nil, nil
	}
	vals := make([]string, len(in))
	copy(vals, in)
	return &substringSet{vals: vals}, nil
}

func (s *substringSet) contains(sv string) bool {
	if s == nil {
		return false
	}
	for _, sub := range s.vals {
		if strings.Contains(sv, sub) {
			return true
		}
	}
	return false
}
