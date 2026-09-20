package e2e_test

import (
	"bytes"
	"context"
	"errors"
	attachmentdom "github.com/Paca-AI/api/internal/domain/attachment"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/Paca-AI/api/internal/platform/storage"
	pg "github.com/Paca-AI/api/internal/repository/postgres"
	attachmentsvc "github.com/Paca-AI/api/internal/service/attachment"
	"github.com/google/uuid"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func startFileDelivery(t *testing.T, env *e2eEnv) *pg.AuthorizationDelivery {
	t.Helper()
	if env.fileStore == nil {
		t.Fatal("real MinIO required")
	}
	d := pg.NewAuthorizationDelivery(env.db, env.redisClient, uuid.NewString())
	stop, err := d.StartParticipant(env.ctx, env.fileGate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	d.Install()
	return d
}

func TestAuthorizedFileDeliveryRealStorage(t *testing.T) {
	env := newE2EEnv(t)
	startFileDelivery(t, env)
	seedTaskMemberUser(t, env, "file-manager", "password1")
	managerClient, managerToken := taskMemberLogin(t, env, "file-manager", "password1")
	project := createProjectForTasksViaAPI(t, env, managerClient, managerToken)
	pid := uuid.MustParse(project)
	manager, _ := env.userRepo.FindByUsername(env.ctx, "file-manager")
	managerCtx := authz.WithActorID(env.ctx, manager.ID)
	seedUser(t, env, "file-reader", "password1", "File Reader")
	user, _ := env.userRepo.FindByUsername(env.ctx, "file-reader")
	member := uuid.New()
	_, err := env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) SELECT $1,$2,$3,id,'human' FROM project_roles WHERE project_id=$2 AND role_name='Editor'`, member, pid, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	client, token := taskMemberLogin(t, env, "file-reader", "password1")
	task, err := env.taskSvc.CreateTask(managerCtx, taskdom.CreateTaskInput{ProjectID: pid, Title: "download", AssigneeIDs: []uuid.UUID{member}})
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("large-content\n"), 900000) // 12.6 MB, above MCP cap.
	session, err := env.attachmentSvc.InitiateUpload(managerCtx, pid, attachmentdom.InitiateUploadInput{TaskID: task.ID, FileName: "active.html", ContentType: "text/html", FileSize: int64(len(data))})
	if err != nil {
		t.Fatal(err)
	}
	if !session.IsMultipart {
		t.Fatal("large upload did not use multipart")
	}
	parts := []attachmentdom.CompletedPart{}
	for _, part := range session.Multipart.Parts {
		start := int64(part.PartNumber-1) * storage.DefaultPartSize
		end := start + storage.DefaultPartSize
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		req, err := http.NewRequestWithContext(env.ctx, "PUT", part.UploadURL, bytes.NewReader(data[start:end]))
		if err != nil {
			t.Fatal("build multipart request failed")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("multipart upload failed (%T)", err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("multipart status=%d", resp.StatusCode)
		}
		parts = append(parts, attachmentdom.CompletedPart{PartNumber: part.PartNumber, ETag: resp.Header.Get("ETag")})
	}
	a, err := env.attachmentSvc.CompleteUpload(managerCtx, pid, attachmentdom.CompleteUploadInput{TaskID: task.ID, FileID: session.FileID, UploadID: &session.Multipart.UploadID, Parts: parts})
	if err != nil {
		t.Fatal(err)
	}
	url, err := env.attachmentSvc.GetDownloadURL(authz.WithActorID(env.ctx, user.ID), pid, task.ID, a.ID, time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(url, "/api/v1/") {
		t.Fatal("download escaped authenticated API")
	}
	request := func(method, path, rg string) *http.Response {
		t.Helper()
		waitFileGate(t, env)
		r := mustRequest(env.ctx, t, method, env.base+path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		if rg != "" {
			r.Header.Set("Range", rg)
		}
		return mustDo(t, client, r)
	}
	t.Run("large_get_head_range_and_mime", func(t *testing.T) {
		for _, tc := range []struct {
			method, rg   string
			status, size int
		}{{"GET", "", 200, len(data)}, {"HEAD", "", 200, 0}, {"GET", "bytes=3-12", 206, 10}, {"GET", "bytes=-9", 206, 9}, {"GET", "bytes=0-1,4-5", 416, -1}} {
			resp := request(tc.method, url, tc.rg)
			body, e := io.ReadAll(resp.Body)
			resp.Body.Close()
			if e != nil || resp.StatusCode != tc.status || (tc.size >= 0 && len(body) != tc.size) {
				t.Fatalf("%s range=%s: status=%d bytes=%d err=%v", tc.method, tc.rg, resp.StatusCode, len(body), e)
			}
			if tc.status < 300 && (!strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") || resp.Header.Get("Cache-Control") != "private, no-store" || resp.Header.Get("X-Content-Type-Options") != "nosniff") {
				t.Fatal(resp.Header)
			}
			if tc.method == "GET" && tc.rg == "" && !bytes.Equal(body, data) {
				t.Fatal("large download body mismatch")
			}
			if tc.rg == "bytes=3-12" && !bytes.Equal(body, data[3:13]) {
				t.Fatal("incorrect range bytes")
			}
		}
		resp := request("GET", strings.TrimSuffix(url, "download")+"content", "")
		defer resp.Body.Close()
		assertStatus(t, resp, 413)
	})
	t.Run("same_url_after_supported_reassignment", func(t *testing.T) {
		empty := []uuid.UUID{}
		_, err := env.taskSvc.UpdateTask(managerCtx, pid, task.ID, taskdom.UpdateTaskInput{AssigneeIDs: &empty})
		if err != nil {
			t.Fatal(err)
		}
		for _, method := range []string{"GET", "HEAD"} {
			resp := request(method, url, "bytes=0-9")
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 404 || bytes.Contains(body, data[:100]) || resp.Header.Get("Content-Disposition") != "" || resp.Header.Get("Content-Range") != "" {
				t.Fatalf("revoked %s status=%d leaked metadata/body", method, resp.StatusCode)
			}
		}
	})
}

func TestFileAssociationMutationAdvancesEpoch(t *testing.T) {
	env := newE2EEnv(t)
	d := startFileDelivery(t, env)
	seedTaskMemberUser(t, env, "unlink-manager", "password1")
	client, token := taskMemberLogin(t, env, "unlink-manager", "password1")
	pid := uuid.MustParse(createProjectForTasksViaAPI(t, env, client, token))
	user, _ := env.userRepo.FindByUsername(env.ctx, "unlink-manager")
	ctx := authz.WithActorID(env.ctx, user.ID)
	task, err := env.taskSvc.CreateTask(ctx, taskdom.CreateTaskInput{ProjectID: pid, Title: "unlink"})
	if err != nil {
		t.Fatal(err)
	}
	f := &attachmentdom.File{ID: uuid.New(), StorageKey: "tasks/fixture", Bucket: "test", FileName: "file", ContentType: "text/plain", FileSize: 1, UploadStatus: attachmentdom.UploadStatusUploaded, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err = env.attachmentRepo.CreateFile(ctx, f); err != nil {
		t.Fatal(err)
	}
	a := &attachmentdom.TaskAttachment{ID: uuid.New(), TaskID: task.ID, FileID: f.ID, CreatedAt: time.Now()}
	if err = env.attachmentRepo.CreateTaskAttachment(ctx, a); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func() error
	}{{"unlink", func() error { return env.attachmentSvc.DeleteTaskAttachment(ctx, pid, task.ID, a.ID) }}, {"delete_file", func() error { return env.attachmentRepo.DeleteFile(ctx, f.ID) }}} {
		t.Run(tc.name, func(t *testing.T) {
			before, e := d.State(ctx)
			if e != nil {
				t.Fatal(e)
			}
			if e = tc.mutate(); e != nil {
				t.Fatal(e)
			}
			after, e := d.State(ctx)
			if e != nil || after.Epoch == before.Epoch || after.Blocked {
				t.Fatalf("mutation did not advance epoch: before=%+v after=%+v err=%v", before, after, e)
			}
		})
	}
}

func waitFileGate(t *testing.T, env *e2eEnv) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for env.fileGate.State().Blocked {
		select {
		case <-deadline:
			t.Fatal("gate did not reopen")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestDocAndScreenshotURLsRecheckCurrentAccess(t *testing.T) {
	env := newE2EEnv(t)
	d := startFileDelivery(t, env)
	seedTaskMemberUser(t, env, "doc-manager", "password1")
	managerClient, managerToken := taskMemberLogin(t, env, "doc-manager", "password1")
	pid := uuid.MustParse(createProjectForTasksViaAPI(t, env, managerClient, managerToken))
	manager, _ := env.userRepo.FindByUsername(env.ctx, "doc-manager")
	ctx := authz.WithActorID(env.ctx, manager.ID)
	seedUser(t, env, "doc-reader", "password1", "Reader")
	user, _ := env.userRepo.FindByUsername(env.ctx, "doc-reader")
	member := uuid.New()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) SELECT $1,$2,$3,id,'human' FROM project_roles WHERE project_id=$2 AND role_name='Editor'`, member, pid, user.ID)
	client, token := taskMemberLogin(t, env, "doc-reader", "password1")
	task, err := env.taskSvc.CreateTask(ctx, taskdom.CreateTaskInput{ProjectID: pid, Title: "screenshot", AssigneeIDs: []uuid.UUID{member}})
	if err != nil {
		t.Fatal(err)
	}
	doc, eid, pfid, annotation := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO documents(visibility,id,project_id,title,content)VALUES('legacy_project',$1,$2,'test','[]')`, doc, pid)
	exec(`INSERT INTO environments(id,project_id,name,slug,backend,secret_key_encrypted)VALUES($1,$2,'test','test','docker','synthetic')`, eid, pid)
	exec(`INSERT INTO environment_port_forwards(id,environment_id,label,container_port)VALUES($1,$2,'test',3000)`, pfid, eid)
	save := func(prefix string) *attachmentdom.File {
		t.Helper()
		f := &attachmentdom.File{ID: uuid.New(), Bucket: "paca-attachments-e2e", FileName: "inert.png", ContentType: "image/png", FileSize: 9, UploadStatus: attachmentdom.UploadStatusUploaded, CreatedAt: time.Now(), UpdatedAt: time.Now()}
		f.StorageKey = prefix + f.ID.String() + "/inert.png"
		if err := env.attachmentRepo.CreateFile(ctx, f); err != nil {
			t.Fatal(err)
		}
		if err := env.fileStore.PutObject(ctx, f.Bucket, f.StorageKey, f.ContentType, []byte("PNG_BYTES")); err != nil {
			t.Fatal(err)
		}
		return f
	}
	df := save("docs/" + doc.String() + "/")
	sf := save("annotations/")
	exec(`INSERT INTO page_annotations(id,project_id,environment_id,port_forward_id,page_path,element_selector,bounding_box,element_snapshot,body,created_by,task_id,screenshot_file_id)VALUES($1,$2,$3,$4,'/','#x','{}','{}','test',$5,$6,$7)`, annotation, pid, eid, pfid, manager.ID, task.ID, sf.ID)
	request := func(method, path, rg string) *http.Response {
		t.Helper()
		waitFileGate(t, env)
		r := mustRequest(env.ctx, t, method, env.base+path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		if rg != "" {
			r.Header.Set("Range", rg)
		}
		return mustDo(t, client, r)
	}
	getURL := func(path string) string {
		t.Helper()
		resp := request("GET", path, "")
		defer resp.Body.Close()
		assertStatus(t, resp, 200)
		var e envelope
		decodeJSON(t, resp, &e)
		url, _ := assertDataMap(t, e)["url"].(string)
		if !strings.HasPrefix(url, "/api/v1/") {
			t.Fatal("not an authenticated API URL")
		}
		return url
	}
	docPath := "/api/v1/projects/" + pid.String() + "/docs/" + doc.String() + "/files/" + df.ID.String()
	annotationPath := "/api/v1/projects/" + pid.String() + "/environments/" + eid.String() + "/port-forwards/" + pfid.String() + "/annotations/" + annotation.String()
	du, su := getURL(docPath+"/download-url"), getURL(annotationPath+"/screenshot-url")
	for _, url := range []string{du, su} {
		for _, method := range []string{"GET", "HEAD"} {
			resp := request(method, url, "bytes=0-2")
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 206 || resp.Header.Get("Content-Range") != "bytes 0-2/9" || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "inline") {
				t.Fatalf("%s status=%d headers=%v", method, resp.StatusCode, resp.Header)
			}
			if method == "GET" && string(b) != "PNG" {
				t.Fatal("bad content")
			}
		}
	}
	// A screenshot replacement is an authority change for an already open source.
	replacement := save("annotations/")
	before, _ := d.State(ctx)
	if err := pg.NewAnnotationRepository(env.db).SetScreenshotFileID(ctx, annotation, replacement.ID); err != nil {
		t.Fatal(err)
	}
	after, _ := d.State(ctx)
	if before.Epoch == after.Epoch {
		t.Error("screenshot replacement did not advance epoch")
	}
	empty := []uuid.UUID{}
	if _, err := env.taskSvc.UpdateTask(ctx, pid, task.ID, taskdom.UpdateTaskInput{AssigneeIDs: &empty}); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "HEAD"} {
		resp := request(method, su, "bytes=0-2")
		resp.Body.Close()
		assertStatus(t, resp, 404)
	}
	// Removing the member is a supported mutation: the old cookie and URL must not retain doc access.
	req := mustRequest(env.ctx, t, "DELETE", env.base+"/api/v1/projects/"+pid.String()+"/members/"+member.String(), nil)
	req.Header.Set("Authorization", "Bearer "+managerToken)
	resp := mustDo(t, managerClient, req)
	resp.Body.Close()
	assertStatus(t, resp, 200)
	for _, method := range []string{"GET", "HEAD"} {
		resp := request(method, du, "bytes=0-2")
		resp.Body.Close()
		assertStatus(t, resp, 403)
		if resp.Header.Get("Content-Disposition") != "" || resp.Header.Get("Content-Range") != "" {
			t.Fatal("revoked doc metadata leaked")
		}
	}
}

func TestFileDeliveryRejectsDeletedSourceAndAssociation(t *testing.T) {
	env := newE2EEnv(t)
	startFileDelivery(t, env)
	seedTaskMemberUser(t, env, "delete-manager", "password1")
	client, token := taskMemberLogin(t, env, "delete-manager", "password1")
	pid := uuid.MustParse(createProjectForTasksViaAPI(t, env, client, token))
	u, _ := env.userRepo.FindByUsername(env.ctx, "delete-manager")
	ctx := authz.WithActorID(env.ctx, u.ID)
	doc := uuid.New()
	if _, err := env.db.Exec(`INSERT INTO documents(visibility,id,project_id,title,content)VALUES('legacy_project',$1,$2,'test','[]')`, doc, pid); err != nil {
		t.Fatal(err)
	}
	task, err := env.taskSvc.CreateTask(ctx, taskdom.CreateTaskInput{ProjectID: pid, Title: "file"})
	if err != nil {
		t.Fatal(err)
	}
	f := &attachmentdom.File{ID: uuid.New(), StorageKey: "docs/" + doc.String() + "/file", Bucket: "paca-attachments-e2e", FileName: "file", ContentType: "text/plain", FileSize: 5, UploadStatus: attachmentdom.UploadStatusUploaded, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := env.attachmentRepo.CreateFile(ctx, f); err != nil {
		t.Fatal(err)
	}
	if err := env.fileStore.PutObject(ctx, f.Bucket, f.StorageKey, f.ContentType, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	tf := *f
	tf.ID = uuid.New()
	tf.StorageKey = "tasks/" + task.ID.String() + "/file"
	if err := env.attachmentRepo.CreateFile(ctx, &tf); err != nil {
		t.Fatal(err)
	}
	if err := env.fileStore.PutObject(ctx, tf.Bucket, tf.StorageKey, tf.ContentType, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	a := &attachmentdom.TaskAttachment{ID: uuid.New(), TaskID: task.ID, FileID: tf.ID, CreatedAt: time.Now()}
	if err := env.attachmentRepo.CreateTaskAttachment(ctx, a); err != nil {
		t.Fatal(err)
	}
	docURL, err := env.attachmentSvc.GetDocFileDownloadURL(ctx, pid, doc, f.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	taskURL, err := env.attachmentSvc.GetDownloadURL(ctx, pid, task.ID, a.ID, time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	get := func(url string, want int) {
		t.Helper()
		waitFileGate(t, env)
		req := mustRequest(ctx, t, "GET", env.base+url, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp := mustDo(t, client, req)
		defer resp.Body.Close()
		assertStatus(t, resp, want)
	}
	get(docURL, 200)
	get(taskURL, 200)
	if err := pg.NewDocumentRepository(env.db).DeleteDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}
	get(docURL, 404)
	if err := env.attachmentSvc.DeleteTaskAttachment(ctx, pid, task.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	get(taskURL, 404)
}

func TestFileMutationRechecksAfterCoordinatorWait(t *testing.T) {
	for _, kind := range []string{"task_unlink", "screenshot_replace", "doc_delete"} {
		t.Run(kind, func(t *testing.T) {
			env := newE2EEnv(t)
			d := startFileDelivery(t, env)
			seedTaskMemberUser(t, env, "race-manager", "password1")
			mc, mt := taskMemberLogin(t, env, "race-manager", "password1")
			pid := uuid.MustParse(createProjectForTasksViaAPI(t, env, mc, mt))
			manager, _ := env.userRepo.FindByUsername(env.ctx, "race-manager")
			ctx := authz.WithActorID(env.ctx, manager.ID)
			seedUser(t, env, "race-reader", "password1", "Reader")
			user, _ := env.userRepo.FindByUsername(env.ctx, "race-reader")
			member := uuid.New()
			exec := func(q string, args ...any) {
				t.Helper()
				if _, e := env.db.Exec(q, args...); e != nil {
					t.Fatal(e)
				}
			}
			exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) SELECT $1,$2,$3,id,'human' FROM project_roles WHERE project_id=$2 AND role_name='Editor'`, member, pid, user.ID)
			client, token := taskMemberLogin(t, env, "race-reader", "password1")
			task, err := env.taskSvc.CreateTask(ctx, taskdom.CreateTaskInput{ProjectID: pid, Title: "race", AssigneeIDs: []uuid.UUID{member}})
			if err != nil {
				t.Fatal(err)
			}
			doc, eid, pfid, ann := uuid.New(), uuid.New(), uuid.New(), uuid.New()
			exec(`INSERT INTO documents(visibility,id,project_id,title,content)VALUES('legacy_project',$1,$2,'test','[]')`, doc, pid)
			exec(`INSERT INTO environments(id,project_id,name,slug,backend,secret_key_encrypted)VALUES($1,$2,'test','test','docker','synthetic')`, eid, pid)
			exec(`INSERT INTO environment_port_forwards(id,environment_id,label,container_port)VALUES($1,$2,'test',3000)`, pfid, eid)
			file := &attachmentdom.File{ID: uuid.New(), StorageKey: "docs/" + doc.String() + "/file", Bucket: "paca-attachments-e2e", FileName: "file", ContentType: "text/plain", FileSize: 5, UploadStatus: attachmentdom.UploadStatusUploaded, UploadedBy: &user.ID, CreatedAt: time.Now(), UpdatedAt: time.Now()}
			if kind == "screenshot_replace" {
				file.StorageKey = "annotations/" + file.ID.String() + "/shot"
				file.UploadStatus = attachmentdom.UploadStatusPending
			}
			if e := env.attachmentRepo.CreateFile(ctx, file); e != nil {
				t.Fatal(e)
			}
			if e := env.fileStore.PutObject(ctx, file.Bucket, file.StorageKey, file.ContentType, []byte("hello")); e != nil {
				t.Fatal(e)
			}
			a := &attachmentdom.TaskAttachment{ID: uuid.New(), TaskID: task.ID, FileID: file.ID, CreatedAt: time.Now()}
			if e := env.attachmentRepo.CreateTaskAttachment(ctx, a); e != nil {
				t.Fatal(e)
			}
			exec(`INSERT INTO page_annotations(id,project_id,environment_id,port_forward_id,page_path,element_selector,bounding_box,element_snapshot,body,created_by,task_id)VALUES($1,$2,$3,$4,'/','#x','{}','{}','test',$5,$6)`, ann, pid, eid, pfid, manager.ID, task.ID)
			method, path := "DELETE", "/api/v1/projects/"+pid.String()+"/tasks/"+task.ID.String()+"/attachments/"+a.ID.String()
			var body *bytes.Buffer
			if kind == "screenshot_replace" {
				method = "POST"
				path = "/api/v1/projects/" + pid.String() + "/environments/" + eid.String() + "/port-forwards/" + pfid.String() + "/annotations/" + ann.String() + "/complete-upload"
				body = jsonBody(t, map[string]any{"file_id": file.ID})
			}
			if kind == "doc_delete" {
				path = "/api/v1/projects/" + pid.String() + "/docs/" + doc.String() + "/files/" + file.ID.String()
			}
			entered, release := make(chan struct{}), make(chan struct{})
			mutation := make(chan error, 1)
			go func() {
				mutation <- d.Mutate(ctx, func(txctx context.Context) error {
					close(entered)
					<-release
					var e error
					if kind == "doc_delete" {
						_, e = pg.DeliveryQuery(txctx, env.db).ExecContext(txctx, `UPDATE project_members SET deleted_at=now() WHERE id=$1`, member)
					} else {
						_, e = pg.DeliveryQuery(txctx, env.db).ExecContext(txctx, `DELETE FROM task_assignees WHERE task_id=$1`, task.ID)
					}
					return e
				})
			}()
			<-entered
			req := mustRequest(env.ctx, t, method, env.base+path, body)
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			response := make(chan *http.Response, 1)
			requestErr := make(chan error, 1)
			go func() {
				resp, e := client.Do(req)
				if e != nil {
					requestErr <- e
				} else {
					response <- resp
				}
			}()
			deadline := time.After(5 * time.Second)
			waiting := false
			for !waiting {
				var n int
				if e := env.db.Get(&n, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory'`); e != nil {
					t.Fatal(e)
				}
				if n > 0 {
					waiting = true
					break
				}
				select {
				case e := <-requestErr:
					close(release)
					t.Fatal(e)
				case resp := <-response:
					resp.Body.Close()
					close(release)
					t.Fatalf("request bypassed coordinator: %d", resp.StatusCode)
				case <-deadline:
					close(release)
					t.Fatal("request did not wait")
				case <-time.After(time.Millisecond):
				}
			}
			if kind == "doc_delete" {
				data, e := env.fileStore.GetObject(ctx, file.Bucket, file.StorageKey, 6)
				if e != nil || string(data) != "hello" {
					t.Error("S3 was deleted before authorized mutation committed")
				}
			}
			close(release)
			if e := <-mutation; e != nil {
				t.Fatal(e)
			}
			select {
			case e := <-requestErr:
				t.Fatal(e)
			case resp := <-response:
				resp.Body.Close()
				if resp.StatusCode != 403 && resp.StatusCode != 404 {
					t.Errorf("stale mutation succeeded: %d", resp.StatusCode)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("stale mutation stuck")
			}
			if _, e := env.attachmentRepo.FindTaskAttachmentByID(ctx, a.ID); e != nil {
				t.Error("stale task unlink changed association")
			}
			f, e := env.attachmentRepo.FindFileByID(ctx, file.ID)
			if e != nil || f.UploadStatus != file.UploadStatus {
				t.Errorf("stale mutation changed file: %+v %v", f, e)
			}
			var replacement *uuid.UUID
			if e := env.db.Get(&replacement, `SELECT screenshot_file_id FROM page_annotations WHERE id=$1`, ann); e != nil {
				t.Fatal(e)
			}
			if replacement != nil {
				t.Error("stale screenshot replacement committed")
			}
		})
	}
}

type failingCleanupStore struct {
	storage.Client
	fail bool
}

func (s *failingCleanupStore) DeleteObject(ctx context.Context, bucket, key string) error {
	if s.fail {
		return errors.New("injected S3 cleanup outage")
	}
	return s.Client.DeleteObject(ctx, bucket, key)
}
func TestDocFileCleanupFailureIsRevokedAndRetryable(t *testing.T) {
	env := newE2EEnv(t)
	startFileDelivery(t, env)
	seedTaskMemberUser(t, env, "cleanup-manager", "password1")
	client, token := taskMemberLogin(t, env, "cleanup-manager", "password1")
	pid := uuid.MustParse(createProjectForTasksViaAPI(t, env, client, token))
	u, _ := env.userRepo.FindByUsername(env.ctx, "cleanup-manager")
	ctx := authz.WithActorID(env.ctx, u.ID)
	doc := uuid.New()
	if _, err := env.db.Exec(`INSERT INTO documents(visibility,id,project_id,title,content)VALUES('legacy_project',$1,$2,'test','[]')`, doc, pid); err != nil {
		t.Fatal(err)
	}
	f := &attachmentdom.File{ID: uuid.New(), Bucket: "paca-attachments-e2e", FileName: "file", ContentType: "text/plain", FileSize: 5, UploadStatus: attachmentdom.UploadStatusUploaded, UploadedBy: &u.ID, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	f.StorageKey = "docs/" + doc.String() + "/" + f.ID.String() + "/file"
	if err := env.attachmentRepo.CreateFile(ctx, f); err != nil {
		t.Fatal(err)
	}
	if err := env.fileStore.PutObject(ctx, f.Bucket, f.StorageKey, f.ContentType, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	url, err := env.attachmentSvc.GetDocFileDownloadURL(ctx, pid, doc, f.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	store := &failingCleanupStore{Client: env.fileStore, fail: true}
	svc := attachmentsvc.New(env.attachmentRepo, env.taskSvc, attachmentsvc.NewDocAccessChecker(pg.NewDocumentRepository(env.db)), store, f.Bucket)
	if err := svc.DeleteDocFile(ctx, pid, doc, f.ID); err == nil || !strings.Contains(err.Error(), "physical cleanup failed") {
		t.Fatal("logical revocation/cleanup failure not reported distinctly", err)
	}
	retained, err := env.attachmentRepo.FindFileByID(ctx, f.ID)
	if err != nil || retained.UploadStatus != attachmentdom.UploadStatusFailed || retained.StorageKey != f.StorageKey {
		t.Fatal("cleanup retry metadata lost", retained, err)
	}
	waitFileGate(t, env)
	req := mustRequest(ctx, t, "GET", env.base+url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp := mustDo(t, client, req)
	resp.Body.Close()
	assertStatus(t, resp, 404)
	if _, err := svc.CompleteDocUpload(ctx, attachmentdom.DocCompleteUploadInput{ProjectID: pid, DocID: doc, FileID: f.ID}); !errors.Is(err, attachmentdom.ErrUploadNotPending) {
		t.Fatal("completion resurrected revoked doc file", err)
	}
	if err := env.attachmentRepo.UpdateFileStatus(ctx, f.ID, attachmentdom.UploadStatusUploaded, nil); err == nil {
		t.Fatal("late completion resurrected revoked file")
	}
	if err := env.attachmentRepo.UpdateFileStatus(ctx, f.ID, attachmentdom.UploadStatusPending, nil); err == nil {
		t.Fatal("late upload initialization resurrected revoked file")
	}
	data, err := env.fileStore.GetObject(ctx, f.Bucket, f.StorageKey, 6)
	if err != nil || string(data) != "hello" {
		t.Fatal("failure injection unexpectedly removed S3 data")
	}
	store.fail = false
	if err := svc.DeleteDocFile(ctx, pid, doc, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.attachmentRepo.FindFileByID(ctx, f.ID); !errors.Is(err, attachmentdom.ErrFileNotFound) {
		t.Fatal("successful cleanup retained file record", err)
	}
	if _, err := env.fileStore.GetObject(ctx, f.Bucket, f.StorageKey, 6); err == nil {
		t.Fatal("successful cleanup retained S3 object")
	}
}
