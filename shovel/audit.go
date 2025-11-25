package shovel

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/indexsupply/shovel/jrpc2"
	"github.com/indexsupply/shovel/shovel/config"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Auditor struct {
	pgp     *pgxpool.Pool
	conf    config.Root
	sources map[string][]*jrpc2.Client
	metrics map[string]*Metrics
	tasks   []*Task
}

func NewAuditor(pgp *pgxpool.Pool, conf config.Root, tasks []*Task) *Auditor {
	a := &Auditor{
		pgp:     pgp,
		conf:    conf,
		tasks:   tasks,
		sources: make(map[string][]*jrpc2.Client),
		metrics: make(map[string]*Metrics),
	}
	for _, sc := range conf.Sources {
		var clients []*jrpc2.Client
		for _, u := range sc.URLs {
			clients = append(clients, jrpc2.New(u))
		}
		a.sources[sc.Name] = clients
		a.metrics[sc.Name] = NewMetrics(sc.Name)
	}
	return a
}

func (a *Auditor) Run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.check(ctx); err != nil {
				slog.ErrorContext(ctx, "audit check", "error", err)
			}
		}
	}
}

type auditTask struct {
	srcName       string
	igName        string
	blockNum      uint64
	consensusHash []byte
}

func (a *Auditor) check(ctx context.Context) error {
	for _, sc := range a.conf.Sources {
		if !sc.Audit.Enabled {
			continue
		}
		// Update queue length metric
		const countQ = `
			SELECT count(*)
			FROM shovel.block_verification bv
			JOIN shovel.task_updates tu ON bv.src_name = tu.src_name AND bv.ig_name = tu.ig_name
			WHERE bv.src_name = $1
			  AND bv.audit_status IN ('pending', 'retrying')
			  AND bv.block_num <= (tu.num - $2)
		`
		var count int64
		if err := a.pgp.QueryRow(ctx, countQ, sc.Name, sc.Audit.Confirmations).Scan(&count); err == nil {
			a.metrics[sc.Name].SetAuditQueueLength(float64(count))
		}

		// Query blocks pending audit for this source
		const q = `
			SELECT bv.src_name, bv.ig_name, bv.block_num, bv.consensus_hash
			FROM shovel.block_verification bv
			JOIN shovel.task_updates tu ON bv.src_name = tu.src_name AND bv.ig_name = tu.ig_name
			WHERE bv.src_name = $1
			  AND bv.audit_status IN ('pending', 'retrying')
			  AND bv.block_num <= (tu.num - $2)
			ORDER BY bv.block_num ASC
			LIMIT 100
		`
		rows, err := a.pgp.Query(ctx, q, sc.Name, sc.Audit.Confirmations)
		if err != nil {
			return fmt.Errorf("querying pending audits: %w", err)
		}
		var batch []auditTask
		for rows.Next() {
			var t auditTask
			if err := rows.Scan(&t.srcName, &t.igName, &t.blockNum, &t.consensusHash); err != nil {
				rows.Close()
				return fmt.Errorf("scanning audit task: %w", err)
			}
			batch = append(batch, t)
		}
		rows.Close()

		if len(batch) == 0 {
			continue
		}

		// Process batch
		// Limit parallelism
		sem := make(chan struct{}, sc.Audit.Parallelism)
		var wg sync.WaitGroup
		for _, t := range batch {
			wg.Add(1)
			sem <- struct{}{}
			go func(t auditTask) {
				defer wg.Done()
				defer func() { <-sem }()
				if err := a.verify(ctx, sc, t); err != nil {
					slog.ErrorContext(ctx, "verifying block",
						"src", t.srcName,
						"ig", t.igName,
						"block", t.blockNum,
						"error", err,
					)
				}
			}(t)
		}
		wg.Wait()
	}
	return nil
}

func (a *Auditor) verify(ctx context.Context, sc config.Source, t auditTask) error {
	// Find the task to get filter
	var task *Task
	for _, tk := range a.tasks {
		if tk.srcName == t.srcName && tk.destConfig.Name == t.igName {
			task = tk
			break
		}
	}
	if task == nil {
		return fmt.Errorf("task not found for %s/%s", t.srcName, t.igName)
	}

	// Pick providers (round-robin based on block num)
	providers := a.sources[sc.Name]
	if len(providers) == 0 {
		return fmt.Errorf("no providers for source %s", sc.Name)
	}
	
	// Fetch from K providers
	k := sc.Audit.ProvidersPerBlock
	if k <= 0 {
		k = 1
	}
	if k > len(providers) {
		k = len(providers)
	}

	a.metrics[sc.Name].AuditAttempt(t.igName)

	// Simple rotation: start at blockNum % len
	startIdx := int(t.blockNum) % len(providers)
	
	var (
		matches = 0
		mu      sync.Mutex
		wg      sync.WaitGroup
	)

	for i := 0; i < k; i++ {
		idx := (startIdx + i) % len(providers)
		p := providers[idx]
		wg.Add(1)
		go func(p *jrpc2.Client) {
			defer wg.Done()
			blocks, err := p.Get(ctx, p.NextURL().String(), &task.filter, t.blockNum, 1)
			if err != nil {
				slog.WarnContext(ctx, "audit fetch failed", "provider", p.NextURL().Hostname(), "error", err)
				return
			}
			h := HashBlocks(blocks)
			if bytes.Equal(h, t.consensusHash) {
				mu.Lock()
				matches++
				mu.Unlock()
			}
		}(p)
	}
	wg.Wait()

	// If all K providers match, mark healthy
	if matches == k {
		const u = `
			UPDATE shovel.block_verification
			SET audit_status = 'healthy', last_verified_at = now()
			WHERE src_name = $1 AND ig_name = $2 AND block_num = $3
		`
		_, err := a.pgp.Exec(ctx, u, t.srcName, t.igName, t.blockNum)
		return err
	}

	// Mismatch or failure -> Trigger Reindex
	slog.WarnContext(ctx, "audit mismatch",
		"src", t.srcName,
		"ig", t.igName,
		"block", t.blockNum,
		"matches", matches,
		"required", k,
	)

	a.metrics[sc.Name].AuditFailure(t.igName)
	a.metrics[sc.Name].ForcedReindex(t.igName)

	// Update status to retrying
	const r = `
		UPDATE shovel.block_verification
		SET audit_status = 'retrying', retry_count = retry_count + 1, last_verified_at = now()
		WHERE src_name = $1 AND ig_name = $2 AND block_num = $3
	`
	if _, err := a.pgp.Exec(ctx, r, t.srcName, t.igName, t.blockNum); err != nil {
		return fmt.Errorf("updating audit status: %w", err)
	}

	// Trigger Delete/Reindex
	// We need a wpg.Conn to pass to Delete. Use pgp.Acquire?
	// Task.Delete takes wpg.Conn.
	conn, err := a.pgp.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquiring conn for delete: %w", err)
	}
	defer conn.Release()

	// *pgxpool.Conn implements wpg.Conn interface
	if err := task.Delete(conn.Conn(), t.blockNum); err != nil {
		return fmt.Errorf("deleting block for reindex: %w", err)
	}

	return nil
}
