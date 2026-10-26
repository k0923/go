package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/k0923/go/dflow"
)

// Provider binds to one immutable publication ID stored separately from runs.
//
// Provider 绑定到一个不可变的 publication ID，该 ID 与 run 分开存储。
type Provider struct {
	db    *sql.DB
	id    string
	codec Codec
}

// Publish stores validated records atomically. Publication IDs are insert-only.
// Same content may be published idempotently; changed content requires a new ID.
//
// Publish 原子存储已校验记录。Publication ID 只能插入。相同内容可以幂等发布；
// 内容变化需要使用新的 ID。
func (i *Infrastructure) Publish(ctx context.Context, id string, graph *dflow.Graph) (*Provider, error) {
	if id == "" || graph == nil {
		return nil, dflow.ErrInvalidDefinition
	}
	m, nodes, err := graph.Publication()
	if err != nil {
		return nil, err
	}
	raw, err := i.codec.Encode(m)
	if err != nil {
		return nil, err
	}
	err = i.transaction(ctx, func(tx *sql.Tx) error {
		var previous []byte
		err := tx.QueryRowContext(ctx, `SELECT manifest FROM dflow_publications WHERE id=?`, id).Scan(&previous)
		if err == nil {
			if string(previous) != string(raw) {
				return dflow.ErrRunMismatch
			}
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO dflow_publications(id,manifest) VALUES(?,?)`, id, raw); err != nil {
			return err
		}
		for _, node := range nodes {
			data, err := i.codec.Encode(node)
			if err != nil {
				return err
			}
			incoming, err := i.codec.Encode(node.Incoming)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO dflow_definitions(publication,node,definition,incoming) VALUES(?,?,?,?)`,
				id, node.Spec.ID, data, incoming); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return i.Provider(id), nil
}

// Provider needs no full definition JSON. Open the DB and bind its publication ID.
//
// Provider 不需要完整 definition JSON。打开数据库并绑定其 publication ID 即可。
func (i *Infrastructure) Provider(id string) *Provider { return &Provider{i.db, id, i.codec} }
func (p *Provider) Manifest(ctx context.Context) (dflow.Manifest, error) {
	var raw []byte
	var m dflow.Manifest
	err := p.db.QueryRowContext(ctx, `SELECT manifest FROM dflow_publications WHERE id=?`, p.id).Scan(&raw)
	if err != nil {
		return m, err
	}
	err = p.codec.Decode(raw, &m)
	return m, err
}
func (p *Provider) LoadNode(ctx context.Context, id string) (*dflow.PublishedNode, error) {
	var raw []byte
	err := p.db.QueryRowContext(ctx, `SELECT definition FROM dflow_definitions WHERE publication=? AND node=?`, p.id, id).Scan(&raw)
	if err != nil {
		return nil, fmt.Errorf("load published node %q: %w", id, err)
	}
	var node dflow.PublishedNode
	err = p.codec.Decode(raw, &node)
	return &node, err
}
func (p *Provider) Incoming(ctx context.Context, id string) ([]string, error) {
	var raw []byte
	err := p.db.QueryRowContext(ctx, `SELECT incoming FROM dflow_definitions WHERE publication=? AND node=?`, p.id, id).Scan(&raw)
	if err != nil {
		return nil, err
	}
	var incoming []string
	err = p.codec.Decode(raw, &incoming)
	return incoming, err
}
