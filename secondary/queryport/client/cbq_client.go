// Temporary implementation to do Create,Drop,Refresh operations on GSI
// cluster. Eventually be replaced by MetadataProvider.

package client

import "net/http"
import "encoding/json"
import "bytes"
import "fmt"
import "io/ioutil"
import "errors"
import "strings"
import "sync"

import "github.com/couchbase/indexing/secondary/common"
import mclient "github.com/couchbase/indexing/secondary/manager/client"

// indexError for a failed index-request.
type indexError struct {
	Code string `json:"code,omitempty"`
	Msg  string `json:"msg,omitempty"`
}

// indexRequest message
type indexRequest struct {
	Version uint64    `json:"version,omitempty"`
	Type    string    `json:"type,omitempty"`
	Index   indexInfo `json:"index,omitempty"`
}

// indexMetaResponse for an indexRequest
type indexMetaResponse struct {
	Version uint64       `json:"version,omitempty"`
	Status  string       `json:"status,omitempty"`
	Indexes []indexInfo  `json:"indexes,omitempty"`
	Errors  []indexError `json:"errors,omitempty"`
}

// cbqClient to access cbq-agent for admin operation on index.
type cbqClient struct {
	rw        sync.RWMutex // protects `indexes` field
	adminport string
	queryport string
	httpc     *http.Client
	indexes   []*mclient.IndexMetadata
	logPrefix string
}

// newCbqClient create cbq-cluster client.
func newCbqClient(cluster string) (*cbqClient, error) {
	cinfo, err := common.NewClusterInfoCache(common.ClusterUrl(cluster), "default" /*pooln*/)
	if err != nil {
		return nil, err
	}
	if err = cinfo.Fetch(); err != nil {
		return nil, err
	}
	nodes := cinfo.GetNodesByServiceType("indexAdmin")
	if l := len(nodes); l < 1 {
		err := fmt.Errorf("cinfo.GetNodesByServiceType() returns %d nodes", l)
		return nil, err
	}
	adminport, err := cinfo.GetServiceAddress(nodes[0], "indexAdmin")
	if err != nil {
		return nil, err
	}
	queryport, err := cinfo.GetServiceAddress(nodes[0], "indexScan")
	if err != nil {
		return nil, err
	}

	b := &cbqClient{
		adminport: "http://" + adminport,
		queryport: queryport,
		httpc:     http.DefaultClient,
	}
	b.logPrefix = fmt.Sprintf("[cbqClient %v]", b.adminport)
	return b, nil
}

// Refresh implement BridgeAccessor{} interface.
func (b *cbqClient) Refresh() ([]*mclient.IndexMetadata, error) {
	var resp *http.Response
	var mresp indexMetaResponse

	// Construct request body.
	req := indexRequest{Type: "list"}
	body, err := json.Marshal(req)
	if err == nil { // Post HTTP request.
		bodybuf := bytes.NewBuffer(body)
		url := b.adminport + "/list"
		common.Infof("%v posting %v to URL %v", b.logPrefix, bodybuf, url)
		resp, err = b.httpc.Post(url, "application/json", bodybuf)
		if err == nil {
			defer resp.Body.Close()
			mresp, err = b.metaResponse(resp)
			if err == nil {
				indexes := make([]*mclient.IndexMetadata, 0)
				for _, info := range mresp.Indexes {
					indexes = append(
						indexes, newIndexMetaData(&info, b.queryport))
				}
				b.rw.Lock()
				defer b.rw.Unlock()
				b.indexes = indexes
				return indexes, nil
			}
			return nil, err
		}
	}
	return nil, err
}

// Nodes implement BridgeAccessor{} interface.
func (b *cbqClient) Nodes() (map[string]string, error) {
	return map[string]string{b.adminport: b.queryport}, nil
}

// CreateIndex implement BridgeAccessor{} interface.
func (b *cbqClient) CreateIndex(
	name, bucket, using, exprType, partnExpr, whereExpr string,
	secExprs []string, isPrimary bool,
	with []byte) (common.IndexDefnId, error) {

	var resp *http.Response
	var mresp indexMetaResponse

	// Construct request body.
	info := indexInfo{
		Name:      name,
		Bucket:    bucket,
		Using:     using,
		ExprType:  exprType,
		PartnExpr: partnExpr,
		WhereExpr: whereExpr,
		SecExprs:  secExprs,
		IsPrimary: isPrimary,
	}
	req := indexRequest{Type: "create", Index: info}
	body, err := json.Marshal(req)
	if err == nil { // Post HTTP request.
		bodybuf := bytes.NewBuffer(body)
		url := b.adminport + "/create"
		common.Infof("%v posting %v to URL %v", b.logPrefix, bodybuf, url)
		resp, err = b.httpc.Post(url, "application/json", bodybuf)
		if err == nil {
			defer resp.Body.Close()
			mresp, err = b.metaResponse(resp)
			if err == nil {
				defnID := mresp.Indexes[0].DefnID
				b.Refresh()
				return common.IndexDefnId(defnID), nil
			}
			return 0, err
		}
	}
	return 0, err
}

// BuildIndexes implement BridgeAccessor{} interface.
func (b *cbqClient) BuildIndexes(defnID []common.IndexDefnId) error {
	panic("cbqClient does not implement build-indexes")
}

// DropIndex implement BridgeAccessor{} interface.
func (b *cbqClient) DropIndex(defnID common.IndexDefnId) error {
	var resp *http.Response

	// Construct request body.
	req := indexRequest{
		Type: "drop", Index: indexInfo{DefnID: uint64(defnID)},
	}
	body, err := json.Marshal(req)
	if err == nil {
		// Post HTTP request.
		bodybuf := bytes.NewBuffer(body)
		url := b.adminport + "/drop"
		common.Infof("%v posting %v to URL %v", b.logPrefix, bodybuf, url)
		resp, err = b.httpc.Post(url, "application/json", bodybuf)
		if err == nil {
			defer resp.Body.Close()
			_, err = b.metaResponse(resp)
			if err == nil {
				b.Refresh()
				return nil
			}
			return err
		}
	}
	return err
}

// GetQueryports implement BridgeAccessor{} interface.
func (b *cbqClient) GetQueryports() (queryports []string) {
	return []string{b.queryport}
}

// GetQueryport implement BridgeAccessor{} interface.
func (b *cbqClient) GetQueryport(
	defnID common.IndexDefnId) (queryport string, ok bool) {
	return b.queryport, true
}

// Close implement BridgeAccessor
func (b *cbqClient) Close() {
	// TODO: do nothing ?
}

// Gather index meta response from http response.
func (b *cbqClient) metaResponse(
	resp *http.Response) (mresp indexMetaResponse, err error) {

	var body []byte
	body, err = ioutil.ReadAll(resp.Body)
	if err == nil {
		if err = json.Unmarshal(body, &mresp); err == nil {
			common.Tracef("%v received raw response %s", b.logPrefix, string(body))
			if strings.Contains(mresp.Status, "error") {
				err = errors.New(mresp.Errors[0].Msg)
			}
		}
	}
	return mresp, err
}

// indexInfo describes an index.
type indexInfo struct {
	Name      string   `json:"name,omitempty"`
	Bucket    string   `json:"bucket,omitempty"`
	DefnID    uint64   `json:"defnID, omitempty"`
	Using     string   `json:"using,omitempty"`
	ExprType  string   `json:"exprType,omitempty"`
	PartnExpr string   `json:"partnExpr,omitempty"`
	SecExprs  []string `json:"secExprs,omitempty"`
	WhereExpr string   `json:"whereExpr,omitempty"`
	IsPrimary bool     `json:"isPrimary,omitempty"`
}

func newIndexMetaData(info *indexInfo, queryport string) *mclient.IndexMetadata {
	defn := &common.IndexDefn{
		DefnId:       common.IndexDefnId(info.DefnID),
		Name:         info.Name,
		Using:        common.IndexType(info.Using),
		Bucket:       info.Bucket,
		IsPrimary:    info.IsPrimary,
		ExprType:     common.ExprType(info.ExprType),
		SecExprs:     info.SecExprs,
		PartitionKey: info.PartnExpr,
	}
	instances := []*mclient.InstanceDefn{
		&mclient.InstanceDefn{
			InstId: common.IndexInstId(info.DefnID), // TODO: defnID as InstID
			State:  common.INDEX_STATE_READY,
			Endpts: []common.Endpoint{common.Endpoint(queryport)},
		},
	}
	imeta := &mclient.IndexMetadata{
		Definition: defn,
		Instances:  instances,
	}
	return imeta
}
