package shovel

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/indexsupply/shovel/shovel/config"
	"github.com/indexsupply/shovel/wpg"
	"github.com/jackc/pgx/v5/pgxpool"
)

type RepairService struct {
	pgp     *pgxpool.Pool
	conf    config.Root
	manager *Manager
}

func NewRepairService(pgp *pgxpool.Pool, conf config.Root, mgr *Manager) *RepairService {
	return &RepairService{
		pgp:     pgp,
		conf:    conf,
		manager: mgr,
	}
}

type RepairRequest struct {
	Source            string `json:"source"`
	Integration       string `json:"integration"`
	StartBlock        uint64 `json:"start_block"`
	EndBlock          uint64 `json:"end_block"`
	ForceAllProviders bool   `json:"force_all_providers"`
	DryRun            bool   `json:"dry_run"`
}

type RepairResponse struct {
	RepairID          string     `json:"repair_id"`
	Source            string     `json:"source"`
	Integration       string     `json:"integration"`
	StartBlock        uint64     `json:"start_block"`
	EndBlock          uint64     `json:"end_block"`
	Status            string     `json:"status"` // in_progress, completed, failed
	BlocksRequested   int        `json:"blocks_requested"`
	BlocksDeleted     int        `json:"blocks_deleted"`
	BlocksReprocessed int        `json:"blocks_reprocessed"`
	BlocksRemaining   int        `json:"blocks_remaining"`
	RowsAffected      int        `json:"rows_affected"` // For dry-run
	Errors            []string   `json:"errors"`
	CreatedAt         time.Time  `json:"created_at"`
	CompletedAt       *time.Time `json:"completed_at,omitempty"`
}

func (rs *RepairService) HandleRepairRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req RepairRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	// Validate
	if req.Source == "" || req.Integration == "" {
		http.Error(w, "source and integration required", http.StatusBadRequest)
		return
	}
	if req.StartBlock > req.EndBlock {
		http.Error(w, "start_block must be <= end_block", http.StatusBadRequest)
		return
	}
	if req.EndBlock-req.StartBlock > 10000 {
		http.Error(w, "Range exceeds maximum of 10,000 blocks", http.StatusBadRequest)
		return
	}

	// Check source and integration exist
	sources, err := rs.conf.AllSources(r.Context(), rs.pgp)
	if err != nil {
		http.Error(w, fmt.Sprintf("Loading sources: %v", err), http.StatusInternalServerError)
		return
	}
	var srcExists bool
	for _, sc := range sources {
		if sc.Name == req.Source {
			srcExists = true
			break
		}
	}
	if !srcExists {
		http.Error(w, "Source not found", http.StatusNotFound)
		return
	}

	integrations, err := rs.conf.AllIntegrations(r.Context(), rs.pgp)
	if err != nil {
		http.Error(w, fmt.Sprintf("Loading integrations: %v", err), http.StatusInternalServerError)
		return
	}
	var igExists bool
	for _, ig := range integrations {
		if ig.Name == req.Integration {
			igExists = true
			break
		}
	}
	if !igExists {
		http.Error(w, "Integration not found", http.StatusNotFound)
		return
	}

	if req.DryRun {
		// Count affected rows
		count, err := rs.countAffectedRows(r.Context(), req.Source, req.Integration, req.StartBlock, req.EndBlock)
		if err != nil {
			http.Error(w, fmt.Sprintf("Dry run failed: %v", err), http.StatusInternalServerError)
			return
		}
		resp := RepairResponse{
			Status:       "dry_run",
			RowsAffected: count,
			CreatedAt:    time.Now(),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}

	// Create repair job
	repairID := uuid.New().String()
	const insertQ = `
		INSERT INTO shovel.repair_jobs (repair_id, src_name, ig_name, start_block, end_block, status)
		VALUES ($1, $2, $3, $4, $5, 'in_progress')
	`
	if _, err := rs.pgp.Exec(r.Context(), insertQ, repairID, req.Source, req.Integration, req.StartBlock, req.EndBlock); err != nil {
		http.Error(w, fmt.Sprintf("Creating repair job: %v", err), http.StatusInternalServerError)
		return
	}

	RepairRequests.WithLabelValues(req.Source, req.Integration).Inc()

	// Log audit
	const auditQ = `
		INSERT INTO shovel.repair_audit (repair_id, requester, src_name, ig_name, start_block, end_block, dry_run)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`
	requester := r.RemoteAddr
	if _, err := rs.pgp.Exec(r.Context(), auditQ, repairID, requester, req.Source, req.Integration, req.StartBlock, req.EndBlock, req.DryRun); err != nil {
		slog.ErrorContext(r.Context(), "audit log failed", "error", err)
	}

	// Start repair in background
	go rs.executeRepair(context.Background(), repairID, req)

	resp := RepairResponse{
		RepairID:        repairID,
		Status:          "in_progress",
		BlocksRequested: int(req.EndBlock - req.StartBlock + 1),
		CreatedAt:       time.Now(),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (rs *RepairService) HandleRepairStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract repair_id from path: /api/v1/repair/{repair_id}
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 5 {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}
	repairID := parts[4]

	const q = `
		SELECT repair_id, src_name, ig_name, start_block, end_block, status, 
		       blocks_deleted, blocks_reprocessed, errors, created_at, completed_at
		FROM shovel.repair_jobs
		WHERE repair_id = $1
	`
	var resp RepairResponse
	var errorsJSON []byte
	err := rs.pgp.QueryRow(r.Context(), q, repairID).Scan(
		&resp.RepairID, &resp.Source, &resp.Integration, &resp.StartBlock, &resp.EndBlock,
		&resp.Status, &resp.BlocksDeleted, &resp.BlocksReprocessed, &errorsJSON,
		&resp.CreatedAt, &resp.CompletedAt,
	)
	if err != nil {
		http.Error(w, "Repair job not found", http.StatusNotFound)
		return
	}

	if len(errorsJSON) > 0 {
		json.Unmarshal(errorsJSON, &resp.Errors)
	}

	resp.BlocksRequested = int(resp.EndBlock - resp.StartBlock + 1)
	resp.BlocksRemaining = resp.BlocksRequested - resp.BlocksReprocessed

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (rs *RepairService) HandleListRepairs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	status := r.URL.Query().Get("status")
	var q string
	var args []interface{}
	if status != "" {
		q = `SELECT repair_id, src_name, ig_name, start_block, end_block, status, 
		            blocks_deleted, blocks_reprocessed, errors, created_at, completed_at
		     FROM shovel.repair_jobs WHERE status = $1 ORDER BY created_at DESC LIMIT 100`
		args = append(args, status)
	} else {
		q = `SELECT repair_id, src_name, ig_name, start_block, end_block, status, 
		            blocks_deleted, blocks_reprocessed, errors, created_at, completed_at
		     FROM shovel.repair_jobs ORDER BY created_at DESC LIMIT 100`
	}

	rows, err := rs.pgp.Query(r.Context(), q, args...)
	if err != nil {
		http.Error(w, fmt.Sprintf("Query failed: %v", err), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var repairs []RepairResponse
	for rows.Next() {
		var resp RepairResponse
		var errorsJSON []byte
		if err := rows.Scan(
			&resp.RepairID, &resp.Source, &resp.Integration, &resp.StartBlock, &resp.EndBlock,
			&resp.Status, &resp.BlocksDeleted, &resp.BlocksReprocessed, &errorsJSON,
			&resp.CreatedAt, &resp.CompletedAt,
		); err != nil {
			http.Error(w, fmt.Sprintf("Scan failed: %v", err), http.StatusInternalServerError)
			return
		}
		if len(errorsJSON) > 0 {
			json.Unmarshal(errorsJSON, &resp.Errors)
		}
		resp.BlocksRequested = int(resp.EndBlock - resp.StartBlock + 1)
		resp.BlocksRemaining = resp.BlocksRequested - resp.BlocksReprocessed
		repairs = append(repairs, resp)
	}

	result := struct {
		Repairs []RepairResponse `json:"repairs"`
		Total   int              `json:"total"`
	}{
		Repairs: repairs,
		Total:   len(repairs),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func (rs *RepairService) countAffectedRows(ctx context.Context, srcName, igName string, start, end uint64) (int, error) {
	// Find the integration to get table name
	integrations, err := rs.conf.AllIntegrations(ctx, rs.pgp)
	if err != nil {
		return 0, err
	}
	var tableName string
	for _, ig := range integrations {
		if ig.Name == igName {
			tableName = ig.Table.Name
			break
		}
	}
	if tableName == "" {
		return 0, fmt.Errorf("integration table not found")
	}

	q := fmt.Sprintf(`
		SELECT COUNT(*) FROM %s
		WHERE src_name = $1 AND ig_name = $2 AND block_num >= $3 AND block_num <= $4
	`, tableName)
	
	var count int
	err = rs.pgp.QueryRow(ctx, q, srcName, igName, start, end).Scan(&count)
	return count, err
}

func (rs *RepairService) executeRepair(ctx context.Context, repairID string, req RepairRequest) {
	// Acquire advisory lock
	lockID := wpg.LockHash(fmt.Sprintf("repair-%s-%s-%d-%d", req.Source, req.Integration, req.StartBlock, req.EndBlock))
	
	const lockQ = `SELECT pg_try_advisory_lock($1)`
	var locked bool
	if err := rs.pgp.QueryRow(ctx, lockQ, lockID).Scan(&locked); err != nil || !locked {
		rs.updateRepairStatus(ctx, repairID, "failed", []string{"Failed to acquire lock"})
		return
	}
	defer func() {
		const unlockQ = `SELECT pg_advisory_unlock($1)`
		rs.pgp.Exec(ctx, unlockQ, lockID)
	}()

	// Find task
	var task *Task
	for _, t := range rs.manager.tasks {
		if t.srcName == req.Source && t.destConfig.Name == req.Integration {
			task = t
			break
		}
	}
	if task == nil {
		rs.updateRepairStatus(ctx, repairID, "failed", []string{"Task not found"})
		return
	}

	// Delete data
	conn, err := rs.pgp.Acquire(ctx)
	if err != nil {
		rs.updateRepairStatus(ctx, repairID, "failed", []string{fmt.Sprintf("Acquire conn: %v", err)})
		return
	}
	defer conn.Release()

	for blockNum := req.StartBlock; blockNum <= req.EndBlock; blockNum++ {
		if err := task.Delete(conn.Conn(), blockNum); err != nil {
			rs.updateRepairStatus(ctx, repairID, "failed", []string{fmt.Sprintf("Delete block %d: %v", blockNum, err)})
			return
		}
	}

	// Update blocks_deleted
	const updateDeletedQ = `UPDATE shovel.repair_jobs SET blocks_deleted = $1 WHERE repair_id = $2`
	rs.pgp.Exec(ctx, updateDeletedQ, int(req.EndBlock-req.StartBlock+1), repairID)

	RepairBlocksReprocessed.WithLabelValues(req.Source, req.Integration).Add(float64(req.EndBlock - req.StartBlock + 1))

	// Mark completed
	rs.updateRepairStatus(ctx, repairID, "completed", nil)
}

func (rs *RepairService) updateRepairStatus(ctx context.Context, repairID, status string, errs []string) {
	var errorsJSON []byte
	if len(errs) > 0 {
		errorsJSON, _ = json.Marshal(errs)
	}
	const q = `
		UPDATE shovel.repair_jobs
		SET status = $1, errors = $2, completed_at = now()
		WHERE repair_id = $3
	`
	rs.pgp.Exec(ctx, q, status, errorsJSON, repairID)
}
