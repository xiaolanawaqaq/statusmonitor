package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestAdminChannelSortRoundTrip(t *testing.T) {
	setupPublicAPI(t, APIConfig{Enabled: false})
	oldToken, oldScheduler := adminTok, scheduler
	adminTok = "sort-test-token"
	scheduler = NewScheduler(store, cfg)
	t.Cleanup(func() { adminTok, scheduler = oldToken, oldScheduler })
	h := newHandler()
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer sort-test-token")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, body := range []string{
		`{"id":"a","name":"A","type":"mock","group":"game","sort":20,"enabled":true}`,
		`{"id":"b","name":"B","type":"mock","group":"web","sort":10,"enabled":true}`,
		`{"id":"c","name":"C","type":"mock","group":"game","enabled":true}`,
	} {
		if w := request("POST", "/api/admin/channels", body); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	var channels []Channel
	if err := json.Unmarshal(request("GET", "/api/admin/channels", "").Body.Bytes(), &channels); err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	sorts := []int{}
	for _, ch := range channels {
		ids = append(ids, ch.ID)
		sorts = append(sorts, ch.Sort)
	}
	if !reflect.DeepEqual(ids, []string{"c1", "b", "a", "c"}) || !reflect.DeepEqual(sorts, []int{1, 10, 20, 1000}) {
		t.Fatalf("ids %v sorts %v", ids, sorts)
	}
	for _, body := range []string{
		`{"id":"b","name":"B renamed","type":"mock","enabled":false}`,
		`{"id":"b","name":"B renamed","type":"mock","enabled":true,"sort":0}`,
	} {
		if w := request("POST", "/api/admin/channels", body); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var ch Channel
		if err := json.Unmarshal(request("GET", "/api/admin/channels/b", "").Body.Bytes(), &ch); err != nil {
			t.Fatal(err)
		}
		if ch.Sort != 10 {
			t.Fatalf("omitted/zero sort did not preserve order: %+v", ch)
		}
	}
	if w := request("POST", "/api/admin/channels", `{"id":"b","name":"B","type":"mock","sort":1001}`); w.Code != 200 {
		t.Fatal(w.Code)
	}
	var snap StatusResponse
	if err := json.Unmarshal(request("GET", "/api/status", "").Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	ids = nil
	for _, ch := range snap.Channels {
		ids = append(ids, ch.ID)
	}
	if !reflect.DeepEqual(ids, []string{"c1", "a", "c", "b"}) {
		t.Fatalf("public API order = %v", ids)
	}
	for _, value := range []string{`-1`, `1.5`, `"4"`, `2147483648`} {
		w := request(http.MethodPost, "/api/admin/channels", `{"id":"b","name":"B","type":"mock","sort":`+value+`}`)
		if w.Code != 400 {
			t.Fatalf("sort %s returned %d", value, w.Code)
		}
	}
}

func TestSeedRespectsConfiguredSort(t *testing.T) {
	s := newTestStore(t)
	chs := []Channel{{ID: "a", Name: "A", Type: "mock", Sort: 50}, {ID: "b", Name: "B", Type: "mock", Sort: 1}, {ID: "c", Name: "C", Type: "mock"}}
	if _, err := s.SeedChannels(chs); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListChannels(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != "b" || got[1].ID != "c" || got[2].ID != "a" || got[1].Sort != 3 {
		t.Fatalf("seed order = %+v", got)
	}
}
