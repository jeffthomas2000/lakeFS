package gateway

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"time"
	"versio-index/auth"
	authmodel "versio-index/auth/model"
	"versio-index/auth/sig"
	"versio-index/block"
	"versio-index/db"
	"versio-index/gateway/errors"
	"versio-index/gateway/operations"
	"versio-index/gateway/serde"
	"versio-index/ident"
	"versio-index/index"
	"versio-index/index/model"

	"golang.org/x/xerrors"

	"github.com/gorilla/mux"
)

const (
	RepoMatch   = "{repo:[a-z0-9]+}"
	KeyMatch    = "{key:.*}"
	BranchMatch = "{branch:[a-z0-9\\-]+}"
)

func getRepo(req *http.Request) string {
	vars := mux.Vars(req)
	return vars["repo"]
}

func getKey(req *http.Request) string {
	vars := mux.Vars(req)
	return vars["key"]
}

func getBranch(req *http.Request) string {
	vars := mux.Vars(req)
	return vars["branch"]
}

type Server struct {
	region      string
	meta        index.Index
	blockStore  block.Adapter
	authService auth.Service

	server *http.Server
	router mux.Router
}

func (s *Server) RegisterOperation(route *mux.Route, handler operations.AuthenticatedOperationHandler) {
	route.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// structure operation
		authOp := s.authenticateOperation(writer, request, handler.GetIntent(), handler.GetArn())
		if authOp == nil {
			return
		}
		// run callback
		handler.Handle(authOp)
	})
}

func (s *Server) RegisterRepoOperation(route *mux.Route, handler operations.RepoOperationHandler) {
	route.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// structure operation
		authOp := s.authenticateOperation(writer, request, handler.GetIntent(), handler.GetArn())
		if authOp == nil {
			return
		}
		// run callback
		handler.Handle(&operations.RepoOperation{
			AuthenticatedOperation: authOp,
			Repo:                   getRepo(request),
		})
	})
}

func (s *Server) RegisterPathOperation(route *mux.Route, handler operations.PathOperationHandler) {
	route.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// structure operation
		authOp := s.authenticateOperation(writer, request, handler.GetIntent(), handler.GetArn())
		if authOp == nil {
			return
		}
		// run callback
		handler.Handle(&operations.PathOperation{
			BranchOperation: &operations.BranchOperation{
				RepoOperation: &operations.RepoOperation{
					AuthenticatedOperation: authOp,
					Repo:                   getRepo(request),
				},
				Branch: getBranch(request),
			},
			Path: getKey(request),
		})
	})
}

func (s *Server) RegisterBranchOperation(route *mux.Route, handler operations.BranchOperationHandler) {
	route.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// structure operation
		authOp := s.authenticateOperation(writer, request, handler.GetIntent(), handler.GetArn())
		if authOp == nil {
			return
		}
		// run callback
		handler.Handle(&operations.BranchOperation{
			RepoOperation: &operations.RepoOperation{
				AuthenticatedOperation: authOp,
				Repo:                   getRepo(request),
			},
			Branch: getBranch(request),
		})
	})
}

func (s *Server) authenticateOperation(writer http.ResponseWriter, request *http.Request, intent authmodel.Permission_Intent, arn string) *operations.AuthenticatedOperation {
	o := &operations.Operation{
		Request:        request,
		ResponseWriter: writer,

		Region:     s.region,
		Index:      s.meta,
		BlockStore: s.blockStore,
		Auth:       s.authService,
	}
	// authenticate
	authContext, err := sig.ParseV4AuthContext(request)
	if err != nil {
		o.EncodeError(errors.Codes.ToAPIErr(errors.ErrAccessDenied))
		return nil
	}

	creds, err := s.authService.GetAPICredentials(authContext.AccessKeyId)
	if err != nil {
		if !xerrors.Is(err, db.ErrNotFound) {
			o.EncodeError(errors.Codes.ToAPIErr(errors.ErrInternalError))
		} else {
			o.EncodeError(errors.Codes.ToAPIErr(errors.ErrAccessDenied))
		}
		return nil
	}

	err = sig.V4Verify(authContext, creds, request)
	if err != nil {
		o.EncodeError(errors.Codes.ToAPIErr(errors.ErrAccessDenied))
		return nil
	}

	// we are verified!
	op := &operations.AuthenticatedOperation{
		Operation:   o,
		ClientId:    creds.GetClientId(),
		SubjectId:   creds.GetEntityId(),
		SubjectType: creds.GetCredentialType(),
	}

	// interpolate arn string
	arn = auth.ResolveARN(arn, mux.Vars(request))

	// authorize
	authResp, err := s.authService.Authorize(&auth.AuthorizationRequest{
		ClientID:   op.ClientId,
		UserID:     op.SubjectId,
		Intent:     intent,
		SubjectARN: arn,
	})
	if err != nil {
		o.EncodeError(errors.Codes.ToAPIErr(errors.ErrInternalError))
		return nil
	}

	if authResp.Error != nil || !authResp.Allowed {
		o.EncodeError(errors.Codes.ToAPIErr(errors.ErrAccessDenied))
		return nil
	}

	// authentication and authorization complete!
	return op
}

func NewServer(region string, meta index.Index, blockStore block.Adapter, authService auth.Service, listenAddr, bareDomain string) *Server {
	r := mux.NewRouter()
	s := &Server{
		meta:        meta,
		region:      region,
		blockStore:  blockStore,
		authService: authService,
		server: &http.Server{
			Handler: r,
			Addr:    listenAddr,
		},
	}

	// path based routing

	// non-bucket-specific endpoints
	serviceEndpoint := r.Host(bareDomain).Subrouter()
	// global level
	s.RegisterOperation(serviceEndpoint.PathPrefix("/").Methods(http.MethodGet), &operations.ListBuckets{})

	// repo level
	pathBasedRepo := serviceEndpoint.PathPrefix(fmt.Sprintf("/%s", RepoMatch)).Subrouter()
	pathBasedRepoWithKey := pathBasedRepo.PathPrefix(fmt.Sprintf("/%s/%s", BranchMatch, KeyMatch)).Subrouter()
	pathBasedRepoWithKey.Methods(http.MethodDelete).HandlerFunc(s.DeleteObject)
	pathBasedRepoWithKey.Methods(http.MethodGet).HandlerFunc(s.GetObject)
	pathBasedRepoWithKey.Methods(http.MethodHead).HandlerFunc(s.HeadObject)
	pathBasedRepoWithKey.Methods(http.MethodPut).HandlerFunc(s.PutObject)

	pathBasedRepo.Methods(http.MethodPut).HandlerFunc(s.CreateBucket)
	pathBasedRepo.
		Methods(http.MethodGet).
		Queries("prefix", "{prefix}", "Prefix", "{prefix}", "Delimiter", "{delimiter}", "delimiter", "{delimiter}").
		HandlerFunc(s.ListObjects)
	pathBasedRepo.Methods(http.MethodDelete).HandlerFunc(s.DeleteBucket)
	pathBasedRepo.Methods(http.MethodHead).HandlerFunc(s.HeadBucket)
	pathBasedRepo.Methods(http.MethodPost).HandlerFunc(s.DeleteObjects)
	pathBasedRepo.Methods(http.MethodPut).HandlerFunc(s.CreateBucket)

	// sub-domain based routing

	subDomainBasedRepo := r.Host(fmt.Sprintf("%s.%s", RepoMatch, bareDomain)).Subrouter()
	// bucket-specific actions that relate to a key
	subDomainBasedRepoWithKey := subDomainBasedRepo.PathPrefix(fmt.Sprintf("/%s/%s", BranchMatch, KeyMatch)).Subrouter()
	subDomainBasedRepoWithKey.Methods(http.MethodDelete).HandlerFunc(s.DeleteObject)
	subDomainBasedRepoWithKey.Methods(http.MethodGet).HandlerFunc(s.GetObject)
	subDomainBasedRepoWithKey.Methods(http.MethodHead).HandlerFunc(s.HeadObject)
	subDomainBasedRepoWithKey.Methods(http.MethodPut).HandlerFunc(s.PutObject)
	// bucket-specific actions that don't relate to a specific key
	subDomainBasedRepo.Path("/").Methods(http.MethodPut).HandlerFunc(s.CreateBucket)
	subDomainBasedRepo.
		Methods(http.MethodGet).
		Queries("prefix", "{prefix}", "Prefix", "{prefix}", "Delimiter", "{delimiter}", "delimiter", "{delimiter}").
		HandlerFunc(s.ListObjects)
	subDomainBasedRepo.Path("/").Methods(http.MethodDelete).HandlerFunc(s.DeleteBucket)
	subDomainBasedRepo.Path("/").Methods(http.MethodHead).HandlerFunc(s.HeadBucket)
	subDomainBasedRepo.Path("/").Methods(http.MethodPost).HandlerFunc(s.DeleteObjects)

	return s
}

func (s *Server) Listen() error {
	return s.server.ListenAndServe()
}

func (s *Server) DeleteBucket(res http.ResponseWriter, req *http.Request) {

}

func (s *Server) CreateBucket(res http.ResponseWriter, req *http.Request) {
	scope := getScope(req)
	repoId := getRepo(req)
	err := s.meta.CreateRepo(scope.Client.GetId(), repoId, index.DefaultBranch)
	if err != nil {
		res.WriteHeader(http.StatusInternalServerError)
		return
	}
	res.WriteHeader(http.StatusCreated)
}

func (s *Server) HeadBucket(res http.ResponseWriter, req *http.Request) {
	scope := getScope(req)
	repoId := getRepo(req)
	_, err := s.meta.GetRepo(scope.Client.GetId(), repoId)
	if xerrors.Is(err, db.ErrNotFound) {
		res.WriteHeader(http.StatusNotFound)
		return
	} else if err != nil {
		res.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func (s *Server) ListBuckets(res http.ResponseWriter, req *http.Request) {

	scope := getScope(req)
	repos, err := s.meta.ListRepos(scope.Client.GetId())
	if err != nil {
		res.WriteHeader(http.StatusInternalServerError)
		return
	}

	// assemble response
	buckets := make([]serde.Bucket, len(repos))
	for i, repo := range repos {
		buckets[i] = serde.Bucket{
			CreationDate: serde.Timestamp(repo.GetCreationDate()),
			Name:         repo.GetRepoId(),
		}
	}

	err = xml.NewEncoder(res).Encode(serde.ListBucketsOutput{
		Buckets: serde.Buckets{Bucket: buckets},
		Owner: serde.Owner{
			DisplayName: scope.Client.GetName(),
			ID:          scope.Client.GetId(),
		},
	})
	if err != nil {
		res.WriteHeader(http.StatusInternalServerError)
		return
	}

}

func (s *Server) ListObjects(res http.ResponseWriter, req *http.Request) {
	//scope := getScope(req)
	//repoId := getRepo(req)
	// get branch and path

	//s.meta.ListObjects(scope.Client.GetId(), repoId, "master", "/")
}

func (s *Server) DeleteObject(res http.ResponseWriter, req *http.Request) {

}

func (s *Server) DeleteObjects(res http.ResponseWriter, req *http.Request) {

}

func (s *Server) GetObject(res http.ResponseWriter, req *http.Request) {
	scope := getScope(req)
	repoId := getRepo(req)
	branch := getBranch(req)
	key := getKey(req)

	obj, err := s.meta.ReadObject(scope.Client.GetId(), repoId, branch, key)
	if xerrors.Is(err, db.ErrNotFound) {
		res.WriteHeader(http.StatusNotFound)
		return
	}
	if err != nil {
		res.WriteHeader(http.StatusInternalServerError)
		return
	}
	blocks := obj.GetBlob().GetBlocks()
	buf := bytes.NewBuffer(nil)
	for _, block := range blocks {
		data, err := s.blockStore.Get(block)
		if err != nil {
			res.WriteHeader(http.StatusInternalServerError)
			return
		}
		buf.Write(data)
	}

	res.Header().Set("Last-Modified", serde.Timestamp(obj.GetTimestamp()))
	res.Header().Set("Etag", ident.Hash(obj))
	// TODO: the rest of https://docs.aws.amazon.com/en_pv/AmazonS3/latest/API/API_GetObject.html

	_, err = io.Copy(res, buf)
	if err != nil {
		res.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func (s *Server) HeadObject(res http.ResponseWriter, req *http.Request) {
	scope := getScope(req)
	clientId := scope.Client.GetId()
	repoId := getRepo(req)
	branch := getBranch(req)
	key := getKey(req)

	obj, err := s.meta.ReadObject(clientId, repoId, branch, key)
	if xerrors.Is(err, db.ErrNotFound) {
		res.WriteHeader(http.StatusNotFound)
		return
	}
	if err != nil {
		res.WriteHeader(http.StatusInternalServerError)
		return
	}

	res.Header().Set("Content-Length", fmt.Sprintf("%d", obj.GetSize()))
	res.Header().Set("Last-Modified", serde.Timestamp(obj.GetTimestamp()))
	res.Header().Set("Etag", ident.Hash(obj))
}

func (s *Server) PutObject(res http.ResponseWriter, req *http.Request) {
	scope := getScope(req)
	repoId := getRepo(req)
	branch := getBranch(req)
	key := getKey(req)

	// handle the upload itself
	data, err := ioutil.ReadAll(req.Body)
	if err != nil {
		res.WriteHeader(http.StatusInternalServerError)
		return
	}

	// write to adapter
	blocks := make([]string, 0)
	blockAddr := ident.Bytes(data)
	err = s.blockStore.Put(data, blockAddr)
	if err != nil {
		res.WriteHeader(http.StatusInternalServerError)
		return
	}
	blocks = append(blocks, blockAddr)

	// write metadata
	err = s.meta.WriteObject(scope.Client.GetId(), repoId, branch, key, &model.Object{
		Blob: &model.Blob{
			Blocks: blocks,
		},
		Metadata:  nil,
		Timestamp: time.Now().Unix(),
		Size:      int64(len(data)),
	})

	if err != nil {
		res.WriteHeader(http.StatusInternalServerError)
		return
	}
	res.WriteHeader(http.StatusCreated)

}

//CreateMultipartUpload
//
//CompleteMultipartUpload
//
//AbortMultipartUpload
//
//ListMultipartUploads
//
//UploadPart