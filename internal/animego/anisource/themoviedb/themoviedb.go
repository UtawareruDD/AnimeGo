package themoviedb

import (
	"fmt"
	"strings"

	"github.com/google/wire"
	"github.com/pkg/errors"

	"github.com/wetor/AnimeGo/internal/api"
	"github.com/wetor/AnimeGo/internal/constant"
	"github.com/wetor/AnimeGo/internal/exceptions"
	"github.com/wetor/AnimeGo/internal/pkg/request"
	pkgExceptions "github.com/wetor/AnimeGo/pkg/exceptions"
	"github.com/wetor/AnimeGo/pkg/log"
	mem "github.com/wetor/AnimeGo/pkg/memorizer"
	"github.com/wetor/AnimeGo/pkg/utils"
)

type Themoviedb struct {
	cacheInit              bool
	cacheParseThemoviedbID mem.Func
	cacheParseAnimeSeason  mem.Func

	*Options
}

var Set = wire.NewSet(
	NewThemoviedb,
)

var bangumiSubjectApi = func(id int) string {
	return fmt.Sprintf("%s/v0/subjects/%d/subjects", constant.BangumiHost, id)
}

func NewThemoviedb(opts *Options) *Themoviedb {
	return &Themoviedb{
		Options: opts,
	}
}

func (a *Themoviedb) Name() string {
	return "Themoviedb"
}

func (a *Themoviedb) RegisterCache() {
	a.cacheInit = true
	a.cacheParseThemoviedbID = mem.Memorized(constant.ThemoviedbBucket, a.Cache.(mem.Memorizer),
		func(params *mem.Params, results *mem.Results) error {
			bangumiID := 0
			if v := params.Get("bangumi_id"); v != nil {
				bangumiID = v.(int)
			}
			entity, err := a.parseThemoviedbID(params.Get("name").(string), bangumiID)
			if err != nil {
				return err
			}
			results.Set("entity", entity)
			return nil
		})

	a.cacheParseAnimeSeason = mem.Memorized(constant.ThemoviedbBucket, a.Cache.(mem.Memorizer),
		func(params *mem.Params, results *mem.Results) error {
			bangumiID := 0
			if v := params.Get("bangumi_id"); v != nil {
				bangumiID = v.(int)
			}
			seasonInfo, err := a.parseAnimeSeason(params.Get("tmdbID").(int), params.Get("airDate").(string), bangumiID)
			if err != nil {
				return err
			}
			results.Set("seasonInfo", seasonInfo)
			return nil
		})
}

func (a *Themoviedb) Search(name string, filters any) (int, error) {
	entity, err := a.parseThemoviedbID(name, getBangumiID(filters))
	if err != nil {
		return 0, errors.Wrap(err, "查询ThemoviedbID失败")
	}
	return entity.ID, nil
}

func (a *Themoviedb) SearchCache(name string, filters any) (int, error) {
	if !a.cacheInit {
		a.RegisterCache()
	}
	results := mem.NewResults("entity", &Entity{})
	params := mem.NewParams("name", name, "bangumi_id", getBangumiID(filters))
	err := a.cacheParseThemoviedbID(params.
		TTL(a.CacheTime), results)
	if err != nil {
		return 0, errors.Wrap(err, "查询ThemoviedbID失败")
	}
	entity := results.Get("entity").(*Entity)
	return entity.ID, nil
}

func (a *Themoviedb) Get(id int, filters any) (any, error) {
	seasonFilters := parseSeasonFilters(filters)
	seasonInfo, err := a.parseAnimeSeason(id, seasonFilters.AirDate, seasonFilters.BangumiID)
	if err != nil {
		return nil, errors.Wrap(err, "获取Themoviedb信息失败")
	}
	return seasonInfo, nil
}

func (a *Themoviedb) GetCache(id int, filters any) (any, error) {
	if !a.cacheInit {
		a.RegisterCache()
	}
	seasonFilters := parseSeasonFilters(filters)
	results := mem.NewResults("seasonInfo", &SeasonInfo{})
	err := a.cacheParseAnimeSeason(mem.NewParams("tmdbID", id, "airDate", seasonFilters.AirDate, "bangumi_id", seasonFilters.BangumiID).
		TTL(a.CacheTime), results)
	if err != nil {
		return nil, errors.Wrap(err, "获取Themoviedb信息失败")
	}
	seasonInfo := results.Get("seasonInfo").(*SeasonInfo)
	return seasonInfo, nil
}

func (a *Themoviedb) parseThemoviedbID(name string, bangumiID int) (entity *Entity, err error) {
	entity, err = a.parseThemoviedbIDWithVisited([]string{name}, bangumiID, nil)
	if err != nil {
		switch errors.Cause(err).(type) {
		case *exceptions.ErrThemoviedbMatchSeason:
			return nil, err
		default:
			if isThemoviedbParseError(err) {
				return nil, &exceptions.ErrThemoviedbSearchName{}
			}
			return nil, err
		}
	}
	return entity, nil
}

func (a *Themoviedb) parseAnimeSeason(tmdbID int, airDate string, bangumiID int) (seasonInfo *SeasonInfo, err error) {
	return a.parseAnimeSeasonWithVisited(tmdbID, airDate, bangumiID, nil)
}

type bangumiSubjectRelation struct {
	ID     int
	Name   string
	NameCN string
}

func (a *Themoviedb) parseThemoviedbIDWithVisited(names []string, bangumiID int, visited map[int]struct{}) (*Entity, error) {
	if visited == nil {
		visited = make(map[int]struct{})
	} else {
		visited = copyVisited(visited)
	}
	if bangumiID > 0 {
		if _, ok := visited[bangumiID]; ok {
			return nil, &pkgExceptions.ErrRemoveNameSuffix{}
		}
		visited[bangumiID] = struct{}{}
	}

	var lastErr error
	for _, name := range uniqueNames(names) {
		entity, err := a.searchThemoviedbID(name)
		if err != nil {
			if isThemoviedbParseError(err) {
				lastErr = err
				continue
			}
			return nil, err
		}
		if entity != nil {
			return entity, nil
		}
	}

	if !a.EnableBacktrace || bangumiID <= 0 {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, &pkgExceptions.ErrRemoveNameSuffix{}
	}

	prequels, err := a.fetchBangumiPrequelSubjects(bangumiID)
	if err != nil {
		log.DebugErr(err)
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, &pkgExceptions.ErrRemoveNameSuffix{}
	}

	for _, prequel := range prequels {
		entity, err := a.parseThemoviedbIDWithVisited([]string{prequel.NameCN, prequel.Name}, prequel.ID, visited)
		if err != nil {
			if isThemoviedbParseError(err) {
				lastErr = err
				continue
			}
			return nil, err
		}
		if entity != nil {
			return entity, nil
		}
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, &pkgExceptions.ErrRemoveNameSuffix{}
}

func (a *Themoviedb) parseAnimeSeasonWithVisited(tmdbID int, airDate string, bangumiID int, visited map[int]struct{}) (*SeasonInfo, error) {
	if visited == nil {
		visited = make(map[int]struct{})
	} else {
		visited = copyVisited(visited)
	}
	if bangumiID > 0 {
		if _, ok := visited[bangumiID]; ok {
			err := errors.WithStack(&exceptions.ErrThemoviedbMatchSeason{Message: "此番剧可能未开播"})
			log.DebugErr(err)
			return nil, err
		}
		visited[bangumiID] = struct{}{}
	}

	resp := InfoResponse{}
	err := request.Get(infoApi(tmdbID, false), &resp)
	if err != nil {
		log.DebugErr(err)
		return nil, errors.WithStack(&exceptions.ErrRequest{Name: a.Name()})
	}
	if resp.Seasons == nil || len(resp.Seasons) == 0 {
		err = errors.WithStack(&exceptions.ErrThemoviedbMatchSeason{Message: "此番剧可能未开播"})
		log.DebugErr(err)
		return nil, err
	}
	seasonInfo := resp.Seasons[0]
	min := 36500
	for _, r := range resp.Seasons {
		if r.Season == 0 || r.EpName == "Specials" {
			continue
		}
		if s := StrTimeSubAbs(r.AirDate, airDate); s < min {
			min = s
			seasonInfo = r
		}
	}
	seasonInfo.ShowID = tmdbID
	if min <= constant.ThemoviedbMatchSeasonDays {
		seasonInfo.EpName = ""
		return seasonInfo, nil
	}

	matchErr := errors.WithStack(&exceptions.ErrThemoviedbMatchSeason{Message: "此番剧可能未开播"})
	log.DebugErr(matchErr)
	if !a.EnableBacktrace || bangumiID <= 0 {
		return nil, matchErr
	}

	prequels, fetchErr := a.fetchBangumiPrequelSubjects(bangumiID)
	if fetchErr != nil {
		log.DebugErr(fetchErr)
		return nil, matchErr
	}
	if len(prequels) == 0 {
		return nil, matchErr
	}

	for _, prequel := range prequels {
		if _, ok := visited[prequel.ID]; ok {
			continue
		}
		nextVisited := copyVisited(visited)
		nextVisited[prequel.ID] = struct{}{}

		candidates := uniqueNames([]string{prequel.NameCN, prequel.Name})
		var entity *Entity
		for _, candidate := range candidates {
			e, err := a.parseThemoviedbID(candidate, prequel.ID)
			if err != nil {
				if isThemoviedbParseError(err) {
					continue
				}
				return nil, err
			}
			if e != nil {
				entity = e
				break
			}
		}
		if entity == nil {
			continue
		}

		info, err := a.parseAnimeSeasonWithVisited(entity.ID, airDate, prequel.ID, nextVisited)
		if err != nil {
			if isThemoviedbParseError(err) {
				continue
			}
			return nil, err
		}
		if info != nil {
			return info, nil
		}
	}

	return nil, matchErr
}

func (a *Themoviedb) searchThemoviedbID(name string) (*Entity, error) {
	resp := FindResponse{}
	result, err := utils.RemoveNameSuffix(name, func(innerName string) (any, error) {
		resp = FindResponse{}
		err := request.Get(idApi(innerName, false), &resp)
		if err != nil {
			log.DebugErr(err)
		}

		if resp.TotalResults == 1 {
			return resp.Result[0], nil
		} else if resp.TotalResults > 1 {
			for _, result := range resp.Result {
				if result.Name == name {
					return result, nil
				}
			}

			temp := &Entity{}
			maxSimilar := float64(0)
			for _, result := range resp.Result {
				similar := utils.SimilarText(result.Name, name)
				if similar > maxSimilar {
					maxSimilar = similar
					temp = result
				}
			}
			if maxSimilar >= constant.ThemoviedbMinSimilar {
				return temp, nil
			}
			err = errors.WithStack(&exceptions.ErrThemoviedbMatchSeason{Message: "番剧名未找到"})
			log.DebugErr(err)
			return nil, err
		}
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	return result.(*Entity), nil
}

func (a *Themoviedb) fetchBangumiPrequelSubjects(bangumiID int) ([]bangumiSubjectRelation, error) {
	if bangumiID <= 0 {
		return nil, nil
	}
	resp := make([]struct {
		Name     string `json:"name"`
		NameCN   string `json:"name_cn"`
		Relation string `json:"relation"`
		ID       int    `json:"id"`
	}, 0)
	err := request.Get(bangumiSubjectApi(bangumiID), &resp)
	if err != nil {
		return nil, err
	}
	subjects := make([]bangumiSubjectRelation, 0, len(resp))
	for _, item := range resp {
		if item.Relation != "前传" {
			continue
		}
		subjects = append(subjects, bangumiSubjectRelation{
			ID:     item.ID,
			Name:   item.Name,
			NameCN: item.NameCN,
		})
	}
	return subjects, nil
}

func getBangumiID(filters any) int {
	if filters == nil {
		return 0
	}
	switch v := filters.(type) {
	case *SearchFilters:
		if v != nil {
			return v.BangumiID
		}
	case SearchFilters:
		return v.BangumiID
	case *SeasonFilters:
		if v != nil {
			return v.BangumiID
		}
	case SeasonFilters:
		return v.BangumiID
	case int:
		return v
	}
	return 0
}

func parseSeasonFilters(filters any) *SeasonFilters {
	switch v := filters.(type) {
	case *SeasonFilters:
		if v != nil {
			return v
		}
	case SeasonFilters:
		return &SeasonFilters{AirDate: v.AirDate, BangumiID: v.BangumiID}
	case string:
		return &SeasonFilters{AirDate: v}
	}
	return &SeasonFilters{}
}

func uniqueNames(names []string) []string {
	result := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if len(name) == 0 {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	return result
}

func copyVisited(src map[int]struct{}) map[int]struct{} {
	if src == nil {
		return make(map[int]struct{})
	}
	dst := make(map[int]struct{}, len(src))
	for k := range src {
		dst[k] = struct{}{}
	}
	return dst
}

func isThemoviedbParseError(err error) bool {
	if err == nil {
		return false
	}
	if exceptions.IsParseFailed(err) {
		return true
	}
	switch errors.Cause(err).(type) {
	case *exceptions.ErrThemoviedbMatchSeason:
		return true
	case *exceptions.ErrThemoviedbSearchName:
		return true
	}
	return false
}

// Check interface is satisfied
var _ api.AniDataSearchGet = &Themoviedb{}
