package gitquery

import (
	"bufio"
	"io"
	"io/ioutil"

	"gopkg.in/src-d/go-mysql-server.v0/sql"

	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/plumbing/object"
)

const (
	blobsMaxSizeKey     = "GITQUERY_BLOBS_MAX_SIZE"
	blobsAllowBinaryKey = "GITQUERY_BLOBS_ALLOW_BINARY"

	b   = 1
	kib = 1024 * b
	mib = 1024 * kib
)

var (
	blobsAllowBinary = getBoolEnv(blobsAllowBinaryKey, false)
	blobsMaxSize     = getIntEnv(blobsMaxSizeKey, 5) * mib
)

type blobsTable struct {
	pool *RepositoryPool
}

var blobsSchema = sql.Schema{
	{Name: "hash", Type: sql.Text, Nullable: false, Source: blobsTableName},
	{Name: "size", Type: sql.Int64, Nullable: false, Source: blobsTableName},
	{Name: "content", Type: sql.Blob, Nullable: false, Source: blobsTableName},
}

var _ sql.PushdownProjectionAndFiltersTable = (*blobsTable)(nil)

func newBlobsTable(pool *RepositoryPool) sql.Table {
	return &blobsTable{pool: pool}
}

func (blobsTable) Resolved() bool {
	return true
}

func (blobsTable) Name() string {
	return blobsTableName
}

func (blobsTable) Schema() sql.Schema {
	return blobsSchema
}

func (r *blobsTable) TransformUp(f func(sql.Node) (sql.Node, error)) (sql.Node, error) {
	return f(r)
}

func (r *blobsTable) TransformExpressionsUp(f func(sql.Expression) (sql.Expression, error)) (sql.Node, error) {
	return r, nil
}

func (r blobsTable) RowIter(_ sql.Session) (sql.RowIter, error) {
	iter := new(blobIter)

	repoIter, err := NewRowRepoIter(r.pool, iter)
	if err != nil {
		return nil, err
	}

	return repoIter, nil
}

func (blobsTable) Children() []sql.Node {
	return nil
}

func (blobsTable) HandledFilters(filters []sql.Expression) []sql.Expression {
	return handledFilters(blobsTableName, blobsSchema, filters)
}

func (r *blobsTable) WithProjectAndFilters(
	session sql.Session,
	_, filters []sql.Expression,
) (sql.RowIter, error) {
	return rowIterWithSelectors(
		session, r.pool, blobsSchema, blobsTableName, filters,
		[]string{"hash"},
		func(selectors selectors) (RowRepoIter, error) {
			if len(selectors["hash"]) == 0 {
				return new(blobIter), nil
			}

			hashes, err := selectors.textValues("hash")
			if err != nil {
				return nil, err
			}

			return &blobsByHashIter{hashes: hashes}, nil
		},
	)
}

type blobIter struct {
	iter *object.BlobIter
}

func (i *blobIter) NewIterator(repo *Repository) (RowRepoIter, error) {
	iter, err := repo.Repo.BlobObjects()
	if err != nil {
		return nil, err
	}

	return &blobIter{iter: iter}, nil
}

func (i *blobIter) Next() (sql.Row, error) {
	o, err := i.iter.Next()
	if err != nil {
		return nil, err
	}

	return blobToRow(o)
}

func (i *blobIter) Close() error {
	if i.iter != nil {
		i.iter.Close()
	}

	return nil
}

type blobsByHashIter struct {
	repo   *Repository
	pos    int
	hashes []string
}

func (i *blobsByHashIter) NewIterator(repo *Repository) (RowRepoIter, error) {
	return &blobsByHashIter{repo, 0, i.hashes}, nil
}

func (i *blobsByHashIter) Next() (sql.Row, error) {
	for {
		if i.pos >= len(i.hashes) {
			return nil, io.EOF
		}

		hash := plumbing.NewHash(i.hashes[i.pos])
		i.pos++
		blob, err := i.repo.Repo.BlobObject(hash)
		if err == plumbing.ErrObjectNotFound {
			continue
		}

		if err != nil {
			return nil, err
		}

		return blobToRow(blob)
	}
}

func (i *blobsByHashIter) Close() error {
	return nil
}

func blobToRow(c *object.Blob) (sql.Row, error) {
	var content []byte
	var isAllowed = blobsAllowBinary
	if !isAllowed {
		ok, err := isBinary(c)
		if err != nil {
			return nil, err
		}
		isAllowed = !ok
	}

	if c.Size <= int64(blobsMaxSize) && isAllowed {
		r, err := c.Reader()
		if err != nil {
			return nil, err
		}

		content, err = ioutil.ReadAll(r)
		if err != nil {
			return nil, err
		}
	}

	return sql.NewRow(
		c.Hash.String(),
		c.Size,
		content,
	), nil
}

const sniffLen = 8000

// isBinary detects if data is a binary value based on:
// http://git.kernel.org/cgit/git/git.git/tree/xdiff-interface.c?id=HEAD#n198
func isBinary(blob *object.Blob) (bool, error) {
	r, err := blob.Reader()
	if err != nil {
		return false, err
	}

	defer r.Close()

	rd := bufio.NewReader(r)
	var i int
	for {
		if i >= sniffLen {
			return false, nil
		}
		i++

		b, err := rd.ReadByte()
		if err == io.EOF {
			return false, nil
		}

		if err != nil {
			return false, err
		}

		if b == 0 {
			return true, nil
		}
	}
}