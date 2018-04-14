// Package wikipage provides utility functions for retrieving informations about Wikipedia articles.
package wikipage

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pkg/errors"
)

// WikiPage represents a article of the English version of Wikipedia.
type WikiPage struct {
	ID       uint32 `json:"pageid"`
	Title    string
	Abstract string `json:"Extract"`
}

// New creates a new RequestHandler.
func New(lang string) (rh RequestHandler) {
	rh = RequestHandler{
		lang, "https://%v.wikipedia.org/w/api.php?action=query&prop=extracts&exlimit=%v&exintro=&explaintext=&exchars=512&format=json&formatversion=2&pageids=%v",
		make(chan request, exlimit*10),
		make(chan struct{}, 1),
	}
	rh.flakeOut()
	return
}

// RequestHandler is a hub from which is possible to retrieve informations about Wikipedia articles.
type RequestHandler struct {
	lang, queryBase string
	requests        chan request
	isSleeping      chan struct{}
}

// FromContext returns a WikiPage from an article ID. It's safe to use concurrently. Warning: in the worst case if there are problems with the Wikipedia API it can block for more than one hour. As such it's advised to setup a timeout with the context.
func (rh RequestHandler) FromContext(ctx context.Context, pageID uint32) (p WikiPage, err error) {
	requests := rh.requests
	chresult := make(chan result, 1)
	uninitErr := errors.New("Error uninitialized")
	for err = uninitErr; err == uninitErr; {
		select {
		case <-rh.isSleeping:
			go rh.wakeUp()
		case requests <- request{pageID, chresult}:
			requests = nil
		case result := <-chresult:
			p, err = result.Page, result.Err
		case <-ctx.Done():
			err = errors.Errorf("Wikipage: the request was terminated prematurely", ctx.Err())
		}
	}
	return
}

//From returns a WikiPage from an article ID. It's safe to use concurrently. Warning: in the worst case if there are problems with the Wikipedia API it can block for more than one hour.
func (rh RequestHandler) From(pageID uint32) (p WikiPage, err error) {
	p, err = rh.FromContext(context.Background(), pageID)
	return
}

type request struct {
	ID       uint32
	ChResult chan result
}

type result struct {
	Page WikiPage
	Err  error
}

func (rh RequestHandler) flakeOut() {
	rh.isSleeping <- struct{}{}
}

func (rh RequestHandler) wakeUp() {
	defer rh.flakeOut()
	timer, alreadyReadFromTimer := time.NewTimer(0), false

	requests := make([]request, 1, exlimit)
	for expLen := len(requests); expLen > 0; expLen = (7*expLen + 9*len(requests) + 8) / 16 { //adaptation of expected length
		requests = requests[:0]
		if !timer.Stop() && !alreadyReadFromTimer { //Reset timer
			<-timer.C
		}
		timer.Reset(time.Second)
		alreadyReadFromTimer = false
	inloop:
		for len(requests) < cap(requests) {
			var r request
			select {
			case r = <-rh.requests:
				//go on
			default:
				if len(requests) >= expLen {
					break inloop
				}
				select {
				case r = <-rh.requests:
					//go on
				case <-timer.C:
					alreadyReadFromTimer = true
					break inloop
				}
			}
			requests = append(requests, r)
		}
		rh.handle(requests)
	}
}

func (rh RequestHandler) handle(requests []request) {
	if len(requests) == 0 {
		return
	}

	pageIDs := make([]uint32, len(requests))
	for i, r := range requests {
		pageIDs[i] = r.ID
	}
	pageID2Page, err := queryPages(queryFrom(rh.queryBase, pageIDs, rh.lang))
	for _, r := range requests {
		var res result
		p, found := pageID2Page[r.ID]
		switch {
		case err != nil:
			res = result{Err: err}
		case !found:
			res = result{Err: errors.WithStack(pageNotFound{r.ID})}
		default:
			res = result{Page: p}
		}
		r.ChResult <- res
	}
}

type pageNotFound struct {
	pageID uint32
}

func (err pageNotFound) Error() string {
	return fmt.Sprintf("Wikipage: Page %v wasn't found", err.pageID)
}

// NotFound checks if current error was issued by a page not found, if so it returns page ID and sets "ok" true, otherwise "ok" is false.
func NotFound(err error) (pageID uint32, ok bool) {
	pnf, ok := errors.Cause(err).(pageNotFound)
	if ok {
		pageID = pnf.pageID
	}
	return
}

func queryPages(query string) (pageID2Page map[uint32]WikiPage, err error) {
	var pd pagesData
	for t := time.Second; t < time.Hour; t *= 2 { //exponential backoff
		pd, err = pagesDataFrom(query)
		if err == nil {
			pageID2Page = assignmentFrom(pd.Query.Pages)
			break
		}
		time.Sleep(t)
	}

	return
}

const exlimit = 20

func queryFrom(base string, pageIDs []uint32, lang string) (query string) {
	stringIds := make([]string, len(pageIDs))
	for i, pageID := range pageIDs {
		stringIds[i] = fmt.Sprint(pageID)
	}
	return fmt.Sprintf(base, lang, exlimit, url.QueryEscape(strings.Join(stringIds, "|")))
}

type pagesData struct {
	Batchcomplete interface{}
	Warnings      interface{}
	Query         struct {
		Pages []mayMissingPage
	}
}

var client = &http.Client{Timeout: time.Minute}

func pagesDataFrom(query string) (pd pagesData, err error) {
	fail := func(e error) (pagesData, error) {
		pd, err = pagesData{}, errors.Wrapf(e, "Wikipage: error with the following query: %v", query)
		return pd, err
	}

	resp, err := client.Get(query)
	if err != nil {
		return fail(err)
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fail(err)
	}

	err = json.Unmarshal(body, &pd)
	if err != nil {
		return fail(err)
	}

	if pd.Batchcomplete == nil {
		return fail(errors.Errorf("Wikipage: incomplete batch with the following query: %v", query))
	}

	if pd.Warnings != nil {
		return fail(errors.Errorf("Wikipage: warnings - %v - with the following query: %v", pd.Warnings, query))
	}

	return
}

type mayMissingPage struct {
	WikiPage
	Missing bool
}

func assignmentFrom(pages []mayMissingPage) (pageID2Page map[uint32]WikiPage) {
	pageID2Page = make(map[uint32]WikiPage, len(pages))
	for _, p := range pages {
		if p.Missing {
			continue
		}
		p.Abstract = strings.Join(strings.Fields(p.Abstract), " ")
		pageID2Page[p.ID] = p.WikiPage
	}
	return
}