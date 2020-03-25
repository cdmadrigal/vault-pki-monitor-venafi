package pki

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"github.com/Venafi/vcert/pkg/certificate"
	"github.com/Venafi/vcert/pkg/verror"
	"github.com/hashicorp/vault/sdk/framework"
	hconsts "github.com/hashicorp/vault/sdk/helper/consts"
	"github.com/hashicorp/vault/sdk/logical"
	"log"
	"strings"
	"time"
)

//Jobs tructure for import queue worker
type Job struct {
	id         int
	entry      string
	roleName   string
	importPath string
	ctx        context.Context
	//req        *logical.Request
	storage logical.Storage
}

//Result tructure for import queue worker
type Result struct {
	job    Job
	result string
}

// This returns the list of queued for import to TPP certificates
func pathImportQueue(b *backend) *framework.Path {
	ret := &framework.Path{
		Pattern: "import-queue/" + framework.GenericNameRegex("role"),

		Callbacks: map[logical.Operation]framework.OperationFunc{
			logical.ReadOperation: b.pathUpdateImportQueue,
			//TODO: add delete operation to stop import queue and delete it
			//TODO: add delete operation to delete particular import record

		},

		HelpSynopsis:    pathImportQueueSyn,
		HelpDescription: pathImportQueueDesc,
	}
	ret.Fields = addNonCACommonFields(map[string]*framework.FieldSchema{})
	return ret
}

func pathImportQueueList(b *backend) *framework.Path {
	ret := &framework.Path{
		Pattern: "import-queue/",
		Callbacks: map[logical.Operation]framework.OperationFunc{
			logical.ListOperation: b.pathFetchImportQueueList,
		},

		HelpSynopsis:    pathImportQueueSyn,
		HelpDescription: pathImportQueueDesc,
	}
	return ret
}

func (b *backend) pathFetchImportQueueList(ctx context.Context, req *logical.Request, data *framework.FieldData) (response *logical.Response, retErr error) {
	roles, err := req.Storage.List(ctx, "import-queue/")
	var entries []string
	if err != nil {
		return nil, err
	}
	for _, role := range roles {
		log.Printf("Getting entry %s", role)
		rawEntry, err := req.Storage.List(ctx, "import-queue/"+role)
		if err != nil {
			return nil, err
		}
		var entry []string
		for _, e := range rawEntry {
			entry = append(entry, fmt.Sprintf("%s: %s", role, e))
		}
		entries = append(entries, entry...)
	}
	return logical.ListResponse(entries), nil
}

func (b *backend) pathUpdateImportQueue(ctx context.Context, req *logical.Request, data *framework.FieldData) (response *logical.Response, retErr error) {
	roleName := data.Get("role").(string)
	log.Printf("Using role: %s", roleName)

	entries, err := req.Storage.List(ctx, "import-queue/"+data.Get("role").(string)+"/")
	if err != nil {
		return nil, err
	}

	return logical.ListResponse(entries), nil
}

func (b *backend) fillImportQueueTask(roleName string, jobs chan Job, storage logical.Storage, conf *logical.BackendConfig) {
	ctx := context.Background()
	replicationState := conf.System.ReplicationState()
	//Checking if we are on master or on the stanby Vault server
	isSlave := !(conf.System.LocalMount() || !replicationState.HasState(hconsts.ReplicationPerformanceSecondary)) ||
		replicationState.HasState(hconsts.ReplicationDRSecondary) ||
		replicationState.HasState(hconsts.ReplicationPerformanceStandby)
	if isSlave {
		log.Println("We're on slave. Sleeping")
		return
	}
	log.Println("We're on master. Starting to import certificates")
	//var err error
	importPath := "import-queue/" + roleName + "/"

	entries, err := storage.List(ctx, importPath)
	if err != nil {
		log.Printf("Could not get queue list from path %s: %s", err, importPath)
		return
	}
	log.Printf("Queue list on path %s has length %v", importPath, len(entries))

	for i, entry := range entries {
		log.Printf("Allocating job for entry %s", entry)
		job := Job{
			id:         i,
			entry:      entry,
			importPath: importPath,
			roleName:   roleName,
			storage:    storage,
			ctx:        ctx,
		}
		jobs <- job
	}
	return
}

func (b *backend) importToTPP(storage logical.Storage, conf *logical.BackendConfig) {
	taskStorage.register("importcontroler", func() {
		b.controlImportQueue(storage, conf)
	}, 1, time.Second)
}

func (b *backend) controlImportQueue(storage logical.Storage, conf *logical.BackendConfig) {
	ctx := context.Background()
	const fillQueuePrefix = "fillqueue-"
	const importWorkersPrefix = "importworkers-"
	roles, err := storage.List(ctx, "role/")
	if err != nil {
		log.Printf("Couldn't get list of roles %s", roles)
		return
	}

	for i := range roles {
		roleName := roles[i]
		//Update role since it's settings may be changed
		role, err := b.getRole(ctx, storage, roleName)
		if err != nil {
			log.Printf("Error getting role %v: %s\n Exiting.", role, err)
			continue
		}
		if role == nil {
			log.Printf("Unknown role %v\n", role)
			continue
		}
		jobs := make(chan Job, 100)

		taskStorage.register(fillQueuePrefix+roleName, func() {
			log.Printf("run queue filler %s", roleName)
			b.fillImportQueueTask(roleName, jobs, storage, conf)
		}, 1, time.Duration(role.TPPImportTimeout)*time.Second)

		taskStorage.register(importWorkersPrefix+roleName, func() {
			b.worker(jobs)
		}, role.TPPImportWorkers, time.Second*3)
	}
	stringInSlice := func(s string, sl []string) bool {
		for i := range sl {
			if sl[i] == s {
				return true
			}
		}
		return false
	}
	for _, taskName := range taskStorage.getTasksNames() {
		if strings.HasPrefix(taskName, importWorkersPrefix) && !stringInSlice(strings.TrimPrefix(taskName, importWorkersPrefix), roles) {
			taskStorage.del(taskName)
		}
		if strings.HasPrefix(taskName, fillQueuePrefix) && !stringInSlice(strings.TrimPrefix(taskName, fillQueuePrefix), roles) {
			taskStorage.del(taskName)
		}
	}
}

func (b *backend) worker(jobs chan Job) {
	for {
		select {
		case job := <-jobs:
			result := Result{job, b.processImportToTPP(job)}
			log.Printf("Job id: %d ### Processed entry: %s , result:\n %v\n", result.job.id, result.job.entry, result.result)
		default:
			return
		}
	}
}

func (b *backend) processImportToTPP(job Job) string {
	ctx := job.ctx
	//req := job.req
	roleName := job.roleName
	storage := job.storage
	entry := job.entry
	id := job.id
	msg := fmt.Sprintf("Job id: %v ###", id)
	importPath := job.importPath
	log.Printf("%s Processing entry %s\n", msg, entry)
	log.Printf("%s Trying to import certificate with SN %s", msg, entry)
	cl, err := b.ClientVenafi(ctx, storage, roleName, "role")
	if err != nil {
		return fmt.Sprintf("%s Could not create venafi client: %s", msg, err)
	}

	certEntry, err := storage.Get(ctx, importPath+entry)
	if err != nil {
		return fmt.Sprintf("%s Could not get certificate from %s: %s", msg, importPath+entry, err)
	}
	if certEntry == nil {
		return fmt.Sprintf("%s Could not get certificate from %s: cert entry not found", msg, importPath+entry)
	}
	block := pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certEntry.Value,
	}

	Certificate, err := x509.ParseCertificate(certEntry.Value)
	if err != nil {
		return fmt.Sprintf("%s Could not get certificate from entry %s: %s", msg, importPath+entry, err)
	}
	//TODO: here we should check for existing CN and set it to DNS or throw error
	cn := Certificate.Subject.CommonName

	certString := string(pem.EncodeToMemory(&block))
	log.Printf("%s Importing cert to %s:\n", msg, cn)

	importReq := &certificate.ImportRequest{
		// if PolicyDN is empty, it is taken from cfg.Zone
		ObjectName:      cn,
		CertificateData: certString,
		PrivateKeyData:  "",
		Password:        "",
		Reconcile:       false,
		CustomFields:    []certificate.CustomField{{Type: certificate.CustomFieldOrigin, Value: "HashiCorp Vault (+)"}},
	}
	importResp, err := cl.ImportCertificate(importReq)
	if err != nil {
		if errors.Is(err, verror.ServerBadDataResponce) || errors.Is(err, verror.UserDataError) {
			//TODO: Here should be renew instead of deletion
			b.deleteCertFromQueue(job)
		}
		return fmt.Sprintf("%s could not import certificate: %s\n", msg, err)

	}
	log.Printf("%s Certificate imported:\n %s", msg, pp(importResp))
	b.deleteCertFromQueue(job)
	return pp(importResp)

}

func (b *backend) deleteCertFromQueue(job Job) {
	ctx := job.ctx
	s := job.storage
	entry := job.entry
	msg := fmt.Sprintf("Job id: %v ###", job.id)
	importPath := job.importPath
	log.Printf("%s Removing certificate from import path %s", msg, importPath+entry)
	err := s.Delete(ctx, importPath+entry)
	if err != nil {
		log.Printf("%s Could not delete %s from queue: %s", msg, importPath+entry, err)
	} else {
		log.Printf("%s Certificate with SN %s removed from queue", msg, entry)
		_, err := s.List(ctx, importPath)
		if err != nil {
			log.Printf("%s Could not get queue list: %s", msg, err)
		}
	}
}

func (b *backend) cleanupImportToTPP(roleName string, ctx context.Context, req *logical.Request) {

	importPath := "import-queue/" + roleName + "/"
	entries, err := req.Storage.List(ctx, importPath)
	if err != nil {
		log.Printf("Could not read from queue: %s", err)
	}
	for _, sn := range entries {
		err = req.Storage.Delete(ctx, importPath+sn)
		if err != nil {
			log.Printf("Could not delete %s from queue: %s", importPath+sn, err)
		} else {
			log.Printf("Deleted %s from queue", importPath+sn)
		}
	}

}

const pathImportQueueSyn = `
Fetch a CA, CRL, CA Chain, or non-revoked certificate.
`

const pathImportQueueDesc = `
This allows certificates to be fetched. If using the fetch/ prefix any non-revoked certificate can be fetched.

Using "ca" or "crl" as the value fetches the appropriate information in DER encoding. Add "/pem" to either to get PEM encoding.

Using "ca_chain" as the value fetches the certificate authority trust chain in PEM encoding.
`
