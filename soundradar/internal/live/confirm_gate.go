package live

import (
	"github.com/znz/soundradar/internal/confirm"
	"github.com/znz/soundradar/internal/dsp"
	"github.com/znz/soundradar/internal/index"
)

// confirmGate is the onset-triggered secondary embedding check wired into
// match.Engine. Mel stays the primary scorer; this only runs when mel is about
// to fire an event. The embedding is projected from the same mel patch that
// scored (not a fresh PCM re-analysis), so short library samples confirm
// correctly under the live window.
type confirmGate struct {
	ix       *index.Index
	an       *dsp.Analyzer
	enabled  bool
	minScore float64
	onsetDB  float64
}

func (g *confirmGate) Accept(id string, _ int, _ float64, melPatch []float32) bool {
	if g == nil || !g.enabled || g.ix == nil {
		return true
	}
	if g.an != nil {
		energies, _ := g.an.FrameEnergy()
		if !confirm.OnsetOK(energies, g.onsetDB) {
			return false
		}
	}
	if len(melPatch) == 0 {
		return false
	}
	emb := confirm.MelEmbed(melPatch)
	win := g.ix.ConfirmScore(id, emb)
	if win < g.minScore {
		return false
	}
	// Reject when another item's confirm embedding is nearly as close — MelEmbed
	// of the winning mel patch alone cannot stop a polluted template monopoly.
	const margin = 0.06
	for _, other := range g.ix.Items() {
		if other == id {
			continue
		}
		if g.ix.ConfirmScore(other, emb) > win-margin {
			return false
		}
	}
	return true
}
