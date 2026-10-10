package booking

import (
	"slices"
	"sort"

	"github.com/google/uuid"

	"busvoyage/internal/store"
)

// SeatPreferences guide l'attribution automatique du « meilleur siège disponible ».
type SeatPreferences struct {
	CategoryCode string `json:"category_code,omitempty" maxLength:"30" doc:"Catégorie souhaitée (ex. vip, premium, standard)"`
	Position     string `json:"position,omitempty" enum:"window,aisle,front,any" doc:"Préférence de placement"`
	Together     bool   `json:"together,omitempty" doc:"Placer les passagers côte à côte si possible"`
}

type candidate struct {
	cell  store.GetDepartureSeatMapRow
	score int
}

// pickSeats choisit n sièges disponibles selon les préférences.
// excluded : sièges déjà choisis explicitement dans la même réservation.
func pickSeats(seatMap []store.GetDepartureSeatMapRow, categories map[uuid.UUID]store.SeatCategory,
	prefs SeatPreferences, n int, excluded map[uuid.UUID]bool) []store.GetDepartureSeatMapRow {

	var maxRow int32
	for _, c := range seatMap {
		maxRow = max(maxRow, c.RowIndex)
	}
	var cands []candidate
	for _, c := range seatMap {
		if c.Kind != "seat" || c.State != "available" || excluded[c.ID] || c.CategoryID == nil {
			continue
		}
		cat := categories[*c.CategoryID]
		if prefs.CategoryCode != "" && cat.Code != prefs.CategoryCode {
			continue
		}
		score := 0
		switch prefs.Position {
		case "window", "aisle":
			if slices.Contains(c.Attributes, prefs.Position) {
				score += 100
			}
		case "front":
			score += int(maxRow-c.RowIndex) * 10
		}
		if slices.Contains(c.Attributes, "legroom") {
			score += 5
		}
		cands = append(cands, candidate{cell: c, score: score})
	}
	if len(cands) < n {
		return nil
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if a.cell.RowIndex != b.cell.RowIndex {
			return a.cell.RowIndex < b.cell.RowIndex
		}
		return a.cell.ColIndex < b.cell.ColIndex
	})

	if prefs.Together && n > 1 {
		// Cherche la meilleure rangée pouvant accueillir tout le groupe.
		byRow := map[int32][]candidate{}
		var rowOrder []int32
		for _, c := range cands {
			if _, ok := byRow[c.cell.RowIndex]; !ok {
				rowOrder = append(rowOrder, c.cell.RowIndex)
			}
			byRow[c.cell.RowIndex] = append(byRow[c.cell.RowIndex], c)
		}
		for _, r := range rowOrder {
			row := byRow[r]
			if len(row) >= n {
				sort.Slice(row, func(i, j int) bool { return row[i].cell.ColIndex < row[j].cell.ColIndex })
				out := make([]store.GetDepartureSeatMapRow, n)
				for i := range n {
					out[i] = row[i].cell
				}
				return out
			}
		}
	}

	out := make([]store.GetDepartureSeatMapRow, n)
	for i := range n {
		out[i] = cands[i].cell
	}
	return out
}
