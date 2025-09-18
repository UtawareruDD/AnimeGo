package themoviedb

import (
	"fmt"

	"github.com/google/wire"
	"github.com/pkg/errors"

	"github.com/wetor/AnimeGo/internal/api"
	"github.com/wetor/AnimeGo/internal/constant"
	"github.com/wetor/AnimeGo/internal/exceptions"
	"github.com/wetor/AnimeGo/internal/pkg/request"
	"github.com/wetor/AnimeGo/pkg/log"
	mem "github.com/wetor/AnimeGo/pkg/memorizer"
	"github.com/wetor/AnimeGo/pkg/utils"
	"github.com/wetor/AnimeGo/third_party/bangumi/res"
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
			entity, err := a.parseThemoviedbID(params.Get("name").(string))
			if err != nil {
				return err
			}
			results.Set("entity", entity)
			return nil
		})

	a.cacheParseAnimeSeason = mem.Memorized(constant.ThemoviedbBucket, a.Cache.(mem.Memorizer),
		func(params *mem.Params, results *mem.Results) error {
			seasonInfo, err := a.parseAnimeSeason(params.Get("tmdbID").(int), seasonFilterFromParams(params))
			if err != nil {
				return err
			}
			results.Set("seasonInfo", seasonInfo)
			return nil
		})
}

func (a *Themoviedb) Search(name string, filters any) (int, error) {
	entity, err := a.parseThemoviedbID(name)
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
	err := a.cacheParseThemoviedbID(mem.NewParams("name", name).
		TTL(a.CacheTime), results)
	if err != nil {
		return 0, errors.Wrap(err, "查询ThemoviedbID失败")
	}
	entity := results.Get("entity").(*Entity)
	return entity.ID, nil
}

func (a *Themoviedb) Get(id int, filters any) (any, error) {
	seasonFilter := normalizeSeasonFilter(filters)
	seasonInfo, err := a.parseAnimeSeason(id, seasonFilter)
	if err != nil {
		return nil, errors.Wrap(err, "获取Themoviedb信息失败")
	}
	return seasonInfo, nil
}

func (a *Themoviedb) GetCache(id int, filters any) (any, error) {
	if !a.cacheInit {
		a.RegisterCache()
	}
	seasonFilter := normalizeSeasonFilter(filters)
	results := mem.NewResults("seasonInfo", &SeasonInfo{})
	err := a.cacheParseAnimeSeason(mem.NewParams(
		"tmdbID", id,
		"airDate", seasonFilter.AirDate,
		"bangumiID", seasonFilter.BangumiID,
		"backtrace", seasonFilter.Backtrace,
	).
		TTL(a.CacheTime), results)
	if err != nil {
		return nil, errors.Wrap(err, "获取Themoviedb信息失败")
	}
	seasonInfo := results.Get("seasonInfo").(*SeasonInfo)
	return seasonInfo, nil
}

func (a *Themoviedb) parseThemoviedbID(name string) (entity *Entity, err error) {
	resp := FindResponse{}
	result, err := utils.RemoveNameSuffix(name, func(innerName string) (any, error) {
		err := request.Get(idApi(innerName, false), &resp)
		if err != nil {
			log.DebugErr(err)
			//return 0, errors.WithStack(&exceptions.ErrRequest{Name: a.Name()})
		}

		if resp.TotalResults == 1 {
			return resp.Result[0], nil
		} else if resp.TotalResults > 1 {
			// 筛选与original name完全相同的番剧
			for _, result := range resp.Result {
				if result.Name == name {
					return result, nil
				}
			}

			// 按照相似度排序筛选
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
		} else {
			// 未找到结果
			return nil, nil
		}
	})
	if err != nil {
		if exceptions.IsParseFailed(err) {
			return nil, &exceptions.ErrThemoviedbSearchName{}
		} else {
			return nil, err
		}
	}
	return result.(*Entity), nil
}

func (a *Themoviedb) parseAnimeSeason(tmdbID int, filter *SeasonFilter) (seasonInfo *SeasonInfo, err error) {
	resp := InfoResponse{}
	err = request.Get(infoApi(tmdbID, false), &resp)
	if err != nil {
		log.DebugErr(err)
		return nil, errors.WithStack(&exceptions.ErrRequest{Name: a.Name()})
	}
	if resp.Seasons == nil || len(resp.Seasons) == 0 {
		err = errors.WithStack(&exceptions.ErrThemoviedbMatchSeason{Message: "此番剧可能未开播"})
		log.DebugErr(err)
		return nil, err
	}
	seasonInfo, min := matchSeasonByAirDate(resp.Seasons, filter.AirDate)
	if min <= constant.ThemoviedbMatchSeasonDays {
		seasonInfo.EpName = ""
		return seasonInfo, nil
	}

	if filter == nil || !filter.Backtrace || filter.BangumiID <= 0 {
		err = errors.WithStack(&exceptions.ErrThemoviedbMatchSeason{Message: "此番剧可能未开播"})
		log.DebugErr(err)
		return nil, err
	}

	subjectCache := make(map[int]*bangumiSubject)
	queue := make([]int, 0, 4)
	visited := make(map[int]struct{})
	enqueue := func(id int) {
		if _, ok := visited[id]; ok {
			return
		}
		visited[id] = struct{}{}
		queue = append(queue, id)
	}
	enqueue(filter.BangumiID)

	for len(queue) > 0 {
		currentID := queue[0]
		queue = queue[1:]

		subject, err := a.fetchBangumiSubjectCached(subjectCache, currentID)
		if err != nil {
			log.DebugErr(err)
			continue
		}
		if subject == nil {
			continue
		}

		if currentID != filter.BangumiID && len(subject.AirDate) > 0 {
			if season, diff := matchSeasonByAirDate(resp.Seasons, subject.AirDate); season != nil && diff <= constant.ThemoviedbMatchSeasonDays {
				seasonInfo = season
				min = diff
				break
			}
		}

		for _, prequelID := range subject.Prequels {
			enqueue(prequelID)
		}
	}

	if min > constant.ThemoviedbMatchSeasonDays || seasonInfo == nil {
		err = errors.WithStack(&exceptions.ErrThemoviedbMatchSeason{Message: "此番剧可能未开播"})
		log.DebugErr(err)
		return nil, err
	}

	seasonInfo.EpName = ""
	return seasonInfo, nil
}

func normalizeSeasonFilter(filters any) *SeasonFilter {
	switch v := filters.(type) {
	case nil:
		return &SeasonFilter{}
	case string:
		return &SeasonFilter{AirDate: v}
	case SeasonFilter:
		return &SeasonFilter{AirDate: v.AirDate, BangumiID: v.BangumiID, Backtrace: v.Backtrace}
	case *SeasonFilter:
		if v == nil {
			return &SeasonFilter{}
		}
		filter := *v
		return &filter
	default:
		return &SeasonFilter{}
	}
}

func seasonFilterFromParams(params *mem.Params) *SeasonFilter {
	filter := &SeasonFilter{}
	if v := params.Get("airDate"); v != nil {
		if airDate, ok := v.(string); ok {
			filter.AirDate = airDate
		}
	}
	if v := params.Get("bangumiID"); v != nil {
		if bangumiID, ok := v.(int); ok {
			filter.BangumiID = bangumiID
		}
	}
	if v := params.Get("backtrace"); v != nil {
		if backtrace, ok := v.(bool); ok {
			filter.Backtrace = backtrace
		}
	}
	return filter
}

func matchSeasonByAirDate(seasons []*SeasonInfo, airDate string) (*SeasonInfo, int) {
	if len(seasons) == 0 {
		return nil, 36500
	}
	if len(airDate) == 0 {
		for _, season := range seasons {
			if season.Season == 0 || season.EpName == "Specials" {
				continue
			}
			return season, 0
		}
		return seasons[0], 0
	}
	min := 36500
	seasonInfo := seasons[0]
	for _, r := range seasons {
		if r.Season == 0 || r.EpName == "Specials" {
			continue
		}
		if s := StrTimeSubAbs(r.AirDate, airDate); s < min {
			min = s
			seasonInfo = r
		}
	}
	if seasonInfo == nil {
		return nil, min
	}
	return seasonInfo, min
}

func (a *Themoviedb) fetchBangumiSubjectCached(cache map[int]*bangumiSubject, id int) (*bangumiSubject, error) {
	if subject, ok := cache[id]; ok {
		return subject, nil
	}
	subject, err := a.getBangumiSubject(id)
	if err != nil {
		return nil, err
	}
	cache[id] = subject
	return subject, nil
}

func (a *Themoviedb) getBangumiSubject(id int) (*bangumiSubject, error) {
	if subject, err := a.loadBangumiSubject(id); err == nil && subject != nil {
		return subject, nil
	}
	return a.fetchBangumiSubject(id)
}

func (a *Themoviedb) loadBangumiSubject(id int) (*bangumiSubject, error) {
	if a.BangumiCache == nil {
		return nil, errors.WithStack(&exceptions.ErrBangumiCacheNotFound{BangumiID: id})
	}
	entity := &bangumiCacheSubject{}
	if a.BangumiCacheLock != nil {
		a.BangumiCacheLock.Lock()
		defer a.BangumiCacheLock.Unlock()
	}
	err := a.BangumiCache.Get(constant.BangumiSubjectBucket, id, entity)
	if err != nil {
		return nil, err
	}
	return entity.toSubject(), nil
}

func (a *Themoviedb) fetchBangumiSubject(id int) (*bangumiSubject, error) {
	resp := res.SubjectV0{}
	err := request.Get(fmt.Sprintf("%s/v0/subjects/%d", constant.BangumiHost, id), &resp)
	if err != nil {
		return nil, errors.WithStack(&exceptions.ErrRequest{Name: "Bangumi"})
	}
	subject := &bangumiSubject{ID: int(resp.ID)}
	if resp.Date != nil {
		subject.AirDate = *resp.Date
	}
	for _, relation := range resp.Relations {
		if relation.Relation == "前传" {
			subject.Prequels = append(subject.Prequels, int(relation.SubjectID))
		}
	}
	return subject, nil
}

type bangumiSubject struct {
	ID       int
	AirDate  string
	Prequels []int
}

type bangumiCacheSubject struct {
	ID        int                    `json:"id"`
	AirDate   string                 `json:"airdate"`
	Relations []bangumiCacheRelation `json:"relations"`
}

type bangumiCacheRelation struct {
	ID       int    `json:"id"`
	Relation string `json:"relation"`
}

func (s *bangumiCacheSubject) toSubject() *bangumiSubject {
	if s == nil {
		return nil
	}
	subject := &bangumiSubject{
		ID:      s.ID,
		AirDate: s.AirDate,
	}
	for _, relation := range s.Relations {
		if relation.Relation == "前传" && relation.ID != 0 {
			subject.Prequels = append(subject.Prequels, relation.ID)
		}
	}
	return subject
}

// Check interface is satisfied
var _ api.AniDataSearchGet = &Themoviedb{}
