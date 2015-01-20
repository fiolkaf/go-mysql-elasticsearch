package river

import (
	"fmt"
	"github.com/siddontang/go-mysql-elasticsearch/elastic"
	"github.com/siddontang/go-mysql/mysql"
	"github.com/siddontang/go/log"
	"time"
)

const (
	syncInsertDoc = iota
	syncDeleteDoc
	syncUpdateDoc
)

// for insert and delete
func (r *River) makeRequest(rule *Rule, dtype int, rows [][]interface{}) ([]*elastic.BulkRequest, error) {
	reqs := make([]*elastic.BulkRequest, 0, len(rows))

	for _, values := range rows {
		if len(rule.TableInfo.Columns) != len(values) {
			return nil, fmt.Errorf("invalid table format for %s, column number is %d, but real data is %d",
				rule.Table, len(rule.TableInfo.Columns), len(values))
		}

		id, err := r.getDocID(rule, values)
		if err != nil {
			return nil, err
		}

		req := &elastic.BulkRequest{Index: rule.Index, Type: rule.Type, ID: id}

		if dtype == syncDeleteDoc {
			req.Action = elastic.ActionDelete
		} else {
			r.makeReqData(req, rule, values)
		}

		reqs = append(reqs, req)
	}

	return reqs, nil
}

func (r *River) makeInsertRequest(rule *Rule, rows [][]interface{}) ([]*elastic.BulkRequest, error) {
	return r.makeRequest(rule, syncInsertDoc, rows)
}

func (r *River) makeDeleteRequest(rule *Rule, rows [][]interface{}) ([]*elastic.BulkRequest, error) {
	return r.makeRequest(rule, syncDeleteDoc, rows)
}

func (r *River) makeUpdateRequest(rule *Rule, rows [][]interface{}) ([]*elastic.BulkRequest, error) {
	if len(rows)%2 != 0 {
		return nil, fmt.Errorf("invalid update rows event, must have 2x rows, but %d", len(rows))
	}

	reqs := make([]*elastic.BulkRequest, 0, len(rows))

	columnCount := len(rule.TableInfo.Columns)
	for i := 0; i < len(rows); i += 2 {
		if columnCount != len(rows[i]) {
			return nil, fmt.Errorf("invalid table format for %s, column number is %d, but real data is %d",
				rule.Table, len(rule.TableInfo.Columns), len(rows[i]))
		}

		beforeID, err := r.getDocID(rule, rows[i])
		if err != nil {
			return nil, err
		}

		afterID, err := r.getDocID(rule, rows[i+1])
		if err != nil {
			return nil, err
		}

		req := &elastic.BulkRequest{Index: rule.Index, Type: rule.Type, ID: beforeID}

		if beforeID != afterID {
			// PK has been changed in update, delete old id first
			req.Action = elastic.ActionDelete
			reqs = append(reqs, req)

			req = &elastic.BulkRequest{Index: rule.Index, Type: rule.Type, ID: afterID}
		}

		r.makeReqData(req, rule, rows[i+1])

		reqs = append(reqs, req)
	}

	return reqs, nil
}

func (r *River) syncDocument(rule *Rule, dtype int, rows [][]interface{}) error {
	if len(rows) == 0 {
		return nil
	}

	var reqs []*elastic.BulkRequest
	var err error

	switch dtype {
	case syncInsertDoc:
		reqs, err = r.makeInsertRequest(rule, rows)
	case syncDeleteDoc:
		reqs, err = r.makeDeleteRequest(rule, rows)
	case syncUpdateDoc:
		reqs, err = r.makeUpdateRequest(rule, rows)
	}

	r.bulkSize.Add(int64(len(reqs)))

	r.ev <- reqs

	return err
}

func (r *River) makeReqData(req *elastic.BulkRequest, rule *Rule, values []interface{}) {
	req.Data = make(map[string]interface{}, len(values))
	req.Action = elastic.ActionIndex

	for i, c := range rule.TableInfo.Columns {
		if values[i] == nil {
			// need to discard nil value ?????
			continue
		}

		if name, ok := rule.FieldMapping[c.Name]; ok {
			// has custom field mapping
			req.Data[name] = values[i]
		} else {
			req.Data[c.Name] = values[i]
		}
	}
}

func (r *River) getDocID(rule *Rule, values []interface{}) (string, error) {
	// now only support one column PK
	id := values[rule.TableInfo.PKColumns[0]]

	if id == nil {
		return "", fmt.Errorf("%s PK is nil for data %v", rule.Table, values)
	}

	return fmt.Sprintf("%v", id), nil
}

func (r *River) syncLoop() {
	defer r.wg.Done()

	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()

	reqs := make([]*elastic.BulkRequest, 0, 1024)

	posUpdate := false
	lastTime := time.Now()
	for {
		select {
		case ev := <-r.ev:
			switch e := ev.(type) {
			case []*elastic.BulkRequest:
				reqs = append(reqs, e...)
				reqs = r.doBulk(reqs, false)
			case mysql.Position:
				reqs = r.doBulk(reqs, true)
				r.m.Update(e.Name, e.Pos)
				posUpdate = true
			}
		case <-t.C:
			reqs = r.doBulk(reqs, true)
		case <-r.quit:
			reqs = r.doBulk(reqs, true)
			if len(r.ev) > 0 {
				log.Warnf("quiting, but at least %d reqs need to been done", len(r.ev))
			}
			return
		}

		if posUpdate {
			n := time.Now()
			if n.Sub(lastTime) > 1*time.Second {
				r.m.Save()
				lastTime = n
			}
			posUpdate = false
		}
	}
}

const maxBulkNum = 100

func (r *River) doBulk(reqs []*elastic.BulkRequest, force bool) []*elastic.BulkRequest {
	if len(reqs) == 0 {
		return reqs
	} else if len(reqs) < maxBulkNum && !force {
		return reqs
	}

	size := len(reqs)
	start := 0
	end := maxBulkNum

	for i := 0; ; i++ {
		start = i * maxBulkNum
		end = (i + 1) * maxBulkNum
		if end > size {
			end = size
		}

		if _, err := r.es.Bulk(reqs[start:end]); err != nil {
			pos := r.m.Pos()
			log.Errorf("sync docs err %v after binlog (%s, %d)", err, pos.Name, pos.Pos)
		}

		if size == end {
			break
		}
	}

	r.bulkSize.Add(int64(-len(reqs)))

	return reqs[0:0]
}

func (r *River) waitPos(pos mysql.Position, seconds int) {
	for i := 0; i < seconds; i++ {
		p := r.m.Pos()
		if p.Compare(pos) >= 0 {
			log.Infof("wait pos %v with %d seconds", pos, i)
			return
		}

		time.Sleep(1 * time.Second)
	}

	log.Warnf("wait pos %v with %d seconds, but now binlog pos is %v", pos, seconds, r.m.Pos())
}