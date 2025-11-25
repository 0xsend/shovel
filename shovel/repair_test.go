package shovel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/indexsupply/shovel/shovel/config"
	"github.com/indexsupply/shovel/tc"
	"github.com/indexsupply/shovel/wpg"
)

func TestHandleRepairRequest(t *testing.T) {
	pg := testpg(t)
	
	// Setup a destination and task
	dest := newTestDestination("bar")
	task := &Task{
		ctx:        context.Background(),
		pgp:        pg,
		srcName:    "foo",
		destConfig: config.Integration{Name: "bar"},
		dests:      []Destination{dest},
	}
	mgr := &Manager{
		tasks: []*Task{task},
	}

	conf := config.Root{
		Sources: []config.Source{{Name: "foo"}},
		Integrations: []config.Integration{
			{
				Name: "bar", 
				Enabled: true,
				Table: wpg.Table{Name: "shovel.bar"},
				Sources: []config.Source{{Name: "foo"}},
			},
		},
	}

	rs := NewRepairService(pg, conf, mgr)

	t.Run("valid request", func(t *testing.T) {
		reqBody := `{"source": "foo", "integration": "bar", "start_block": 100, "end_block": 105}`
		req := httptest.NewRequest("POST", "/api/v1/repair", strings.NewReader(reqBody))
		w := httptest.NewRecorder()

		rs.HandleRepairRequest(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200 OK, got %d body: %s", w.Code, w.Body.String())
		}

		var resp RepairResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}

		if resp.Status != "in_progress" {
			t.Errorf("expected status in_progress, got %s", resp.Status)
		}
		if resp.BlocksRequested != 6 {
			t.Errorf("expected 6 blocks, got %d", resp.BlocksRequested)
		}

		// Wait for async execution (simple sleep for test)
		// Better: poll the DB or status endpoint
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			// check status via DB
			var status string
			err := pg.QueryRow(context.Background(), "select status from shovel.repair_jobs where repair_id=$1", resp.RepairID).Scan(&status)
			if err == nil && status == "completed" {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		
		// Check final status
		reqStatus := httptest.NewRequest("GET", "/api/v1/repair/"+resp.RepairID, nil)
		wStatus := httptest.NewRecorder()
		rs.HandleRepairStatus(wStatus, reqStatus)
		
		if wStatus.Code != http.StatusOK {
			t.Errorf("status: expected 200 OK, got %d", wStatus.Code)
		}
		var statusResp RepairResponse
		json.NewDecoder(wStatus.Body).Decode(&statusResp)
		
		if statusResp.Status != "completed" {
			t.Errorf("expected completed, got %s (errors: %v)", statusResp.Status, statusResp.Errors)
		}
		if statusResp.BlocksDeleted != 6 {
			t.Errorf("expected 6 blocks deleted, got %d", statusResp.BlocksDeleted)
		}
	})

	t.Run("invalid source", func(t *testing.T) {
		reqBody := `{"source": "invalid", "integration": "bar", "start_block": 100, "end_block": 105}`
		req := httptest.NewRequest("POST", "/api/v1/repair", strings.NewReader(reqBody))
		w := httptest.NewRecorder()

		rs.HandleRepairRequest(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", w.Code)
		}
	})

	t.Run("range too large", func(t *testing.T) {
		reqBody := `{"source": "foo", "integration": "bar", "start_block": 100, "end_block": 20000}`
		req := httptest.NewRequest("POST", "/api/v1/repair", strings.NewReader(reqBody))
		w := httptest.NewRecorder()

		rs.HandleRepairRequest(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})
}

func TestRepairList(t *testing.T) {
	pg := testpg(t)
	rs := NewRepairService(pg, config.Root{}, nil)

	// Insert some dummy jobs
	_, err := pg.Exec(context.Background(), `
		INSERT INTO shovel.repair_jobs (repair_id, src_name, ig_name, start_block, end_block, status, created_at)
		VALUES 
		('id1', 'foo', 'bar', 10, 20, 'completed', now()),
		('id2', 'foo', 'bar', 30, 40, 'in_progress', now())
	`)
	tc.NoErr(t, err)

	req := httptest.NewRequest("GET", "/api/v1/repairs", nil)
	w := httptest.NewRecorder()
	rs.HandleListRepairs(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var res struct {
		Repairs []RepairResponse `json:"repairs"`
		Total   int              `json:"total"`
	}
	json.NewDecoder(w.Body).Decode(&res)

	if res.Total != 2 {
		t.Errorf("expected 2 repairs, got %d", res.Total)
	}
}

func TestAffectedRows(t *testing.T) {
	pg := testpg(t)
	ctx := context.Background()
	
	// Create table
	_, err := pg.Exec(ctx, "create table shovel.bar (src_name text, ig_name text, block_num bigint)")
	tc.NoErr(t, err)
	
	// Insert data
	_, err = pg.Exec(ctx, "insert into shovel.bar values ('foo', 'bar', 10), ('foo', 'bar', 11), ('foo', 'baz', 10)")
	tc.NoErr(t, err)

	conf := config.Root{
		Integrations: []config.Integration{
			{Name: "bar", Table: wpg.Table{Name: "shovel.bar"}},
		},
	}
	rs := NewRepairService(pg, conf, nil)

	count, err := rs.countAffectedRows(ctx, "foo", "bar", 5, 15)
	tc.NoErr(t, err)
	if count != 2 {
		t.Errorf("expected 2 rows, got %d", count)
	}
}
