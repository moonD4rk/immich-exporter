package exporter

import (
	"context"
	"iter"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

type peopleSnap struct {
	total, hidden, named, unnamed, withBirthdate float64
	ok                                           bool
	assets                                       []labeledVal // per-person asset counts, top-N
}

type person struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	BirthDate *string `json:"birthDate"`
}

type peoplePage struct {
	People      []person `json:"people"`
	HasNextPage bool     `json:"hasNextPage"`
	Total       float64  `json:"total"`
	Hidden      float64  `json:"hidden"`
}

var (
	// people
	dPeople          = desc("people", "Total number of people (including hidden).")
	dPeopleHidden    = desc("people_hidden", "Number of people marked hidden.")
	dPeopleNamed     = desc("people_named", "Number of named (identified) people.")
	dPeopleUnnamed   = desc("people_unnamed", "Number of unnamed people (detected but unlabelled).")
	dPeopleBirthdate = desc("people_with_birthdate", "Number of people with a birth date set.")
	dPersonAssets    = desc("person_assets", "Assets per person (top-N by asset count).", "person_id", "person")
)

// peoplePages walks GET /people one page at a time. The first error is yielded
// and ends the walk, as does the last page or the caller breaking out.
func (p *poller) peoplePages(ctx context.Context) iter.Seq2[peoplePage, error] {
	return func(yield func(peoplePage, error) bool) {
		for page := 1; ; page++ {
			var pg peoplePage
			err := p.c.Get(ctx, "/people?withHidden=true&size=1000&page="+strconv.Itoa(page), &pg)
			if !yield(pg, err) || err != nil || !pg.HasNextPage || len(pg.People) == 0 {
				return
			}
		}
	}
}

// collectPeople fills the people counts and returns the full list so the
// slow-tier person breakdown can reuse it instead of paging /people again.
func (p *poller) collectPeople(ctx context.Context, s *snapshot) ([]person, error) {
	var all []person
	for pg, err := range p.peoplePages(ctx) {
		if err != nil {
			return nil, err
		}
		if !s.people.ok {
			s.people.total, s.people.hidden = pg.Total, pg.Hidden
			s.people.ok = true
		}
		all = append(all, pg.People...)
	}
	var named, withBirthdate float64
	for _, pe := range all {
		if strings.TrimSpace(pe.Name) != "" {
			named++
		}
		if pe.BirthDate != nil {
			withBirthdate++
		}
	}
	s.people.named = named
	s.people.unnamed = s.people.total - named
	s.people.withBirthdate = withBirthdate
	return all, nil
}

// collectPersonStats fails only when every per-person call did, so one person
// the key can't read doesn't hide the rest.
func (p *poller) collectPersonStats(ctx context.Context, people []person, s *snapshot) error {
	// Cap to FanoutLimit, matching the bound fanout() enforces for cameras.
	people = people[:min(len(people), p.cfg.FanoutLimit)]
	var lastErr atomic.Pointer[error]
	vals := parallel(people, p.cfg.FanoutConcurrency, func(pe person) (labeledVal, bool) {
		var st struct {
			Assets float64 `json:"assets"`
		}
		if err := p.c.Get(ctx, "/people/"+pe.ID+"/statistics", &st); err != nil {
			lastErr.Store(&err)
			return labeledVal{}, false
		}
		name := pe.Name
		if name == "" {
			name = "(unnamed)"
		}
		return labeledVal{id: pe.ID, name: name, value: st.Assets}, true
	})
	if len(vals) == 0 && lastErr.Load() != nil {
		return *lastErr.Load()
	}
	s.people.assets = topNLabeled(vals, p.cfg.TopN)
	return nil
}

func emitPeople(ch chan<- prometheus.Metric, s *snapshot) {
	p := &s.people
	if p.ok {
		emit(ch, dPeople, p.total)
		emit(ch, dPeopleHidden, p.hidden)
		emit(ch, dPeopleNamed, p.named)
		emit(ch, dPeopleUnnamed, p.unnamed)
		emit(ch, dPeopleBirthdate, p.withBirthdate)
	}
	for _, pe := range p.assets {
		emit(ch, dPersonAssets, pe.value, pe.id, pe.name)
	}
}
