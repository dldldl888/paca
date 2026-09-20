package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	annotationdom "github.com/Paca-AI/api/internal/domain/annotation"
	attachmentdom "github.com/Paca-AI/api/internal/domain/attachment"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/events"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/Paca-AI/api/internal/platform/storage"
	pg "github.com/Paca-AI/api/internal/repository/postgres"
	annotationsvc "github.com/Paca-AI/api/internal/service/annotation"
	attachmentsvc "github.com/Paca-AI/api/internal/service/attachment"
	environmentsvc "github.com/Paca-AI/api/internal/service/environment"
	"github.com/google/uuid"
	"strings"
	"testing"
	"time"
)

func TestTaskBoundAnnotationSQLVisibility(t *testing.T) {
	env := newE2EEnv(t)
	seedTaskMemberUser(t, env, "annotation-manager", "password1")
	client, token := taskMemberLogin(t, env, "annotation-manager", "password1")
	pid := uuid.MustParse(createProjectForTasksViaAPI(t, env, client, token))
	manager, _ := env.userRepo.FindByUsername(env.ctx, "annotation-manager")
	managerCtx := authz.WithActorID(env.ctx, manager.ID)
	seedUser(t, env, "annotation-a", "password1", "A")
	user, _ := env.userRepo.FindByUsername(env.ctx, "annotation-a")
	ctx := authz.WithActorID(env.ctx, user.ID)
	member := uuid.New()
	if _, err := env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) SELECT $1,$2,$3,id,'human' FROM project_roles WHERE project_id=$2 AND role_name='Editor'`, member, pid, user.ID); err != nil {
		t.Fatal(err)
	}
	hidden, err := env.taskSvc.CreateTask(managerCtx, taskdom.CreateTaskInput{ProjectID: pid, Title: "hidden"})
	if err != nil {
		t.Fatal(err)
	}
	eid, pfid := uuid.New(), uuid.New()
	if _, err := env.db.Exec(`INSERT INTO environments(id,project_id,name,slug,backend,secret_key_encrypted) VALUES($1,$2,'test','test','docker','synthetic')`, eid, pid); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`INSERT INTO environment_port_forwards(id,environment_id,label,container_port) VALUES($1,$2,'test',3000)`, pfid, eid); err != nil {
		t.Fatal(err)
	}
	ids := [2]uuid.UUID{uuid.New(), uuid.New()}
	for i, id := range ids {
		var taskID *uuid.UUID
		if i == 1 {
			taskID = &hidden.ID
		}
		if _, err := env.db.Exec(`INSERT INTO page_annotations(id,project_id,environment_id,port_forward_id,page_path,element_selector,bounding_box,element_snapshot,body,created_by,task_id,created_at) VALUES($1,$2,$3,$4,'/','#x','{}','{}',$5,$6,$7,now()+$8::interval)`, id, pid, eid, pfid, "secret-marker", manager.ID, taskID, []string{"0 seconds", "1 seconds"}[i]); err != nil {
			t.Fatal(err)
		}
	}
	store := &boundaryStorage{}
	repo := pg.NewAnnotationRepository(env.db)
	svc := annotationsvc.New(repo, environmentsvc.New(pg.NewEnvironmentRepository(env.db), "", ""), env.taskSvc, env.attachmentRepo, env.attachmentRepo, store, "test").WithTaskAccess(env.taskSvc, env.taskSvc)
	limit := 1
	search := "secret-marker"
	rows, more, err := svc.SearchInProject(ctx, pid, annotationdom.SearchFilter{Limit: &limit, Search: &search})
	if err != nil || more || len(rows) != 1 || rows[0].ID != ids[0] {
		t.Fatalf("hidden annotation affected paginated SQL: rows=%+v more=%v err=%v", rows, more, err)
	}

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := env.db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := env.db.Get(&n, query, args...); err != nil {
			t.Fatal(err)
		}
		return n
	}
	shot, replacement := uuid.New(), uuid.New()
	for _, file := range []*attachmentdom.File{
		{ID: shot, StorageKey: "annotations/" + shot.String() + "/shot.png", Bucket: "test", FileName: "shot.png", ContentType: "image/png", FileSize: 1, UploadStatus: attachmentdom.UploadStatusUploaded, UploadedBy: &manager.ID, CreatedAt: time.Now(), UpdatedAt: time.Now()},
		{ID: replacement, StorageKey: "annotations/" + replacement.String() + "/shot.png", Bucket: "test", FileName: "shot.png", ContentType: "image/png", FileSize: 1, UploadStatus: attachmentdom.UploadStatusPending, UploadedBy: &user.ID, CreatedAt: time.Now(), UpdatedAt: time.Now()},
	} {
		if err := env.attachmentRepo.CreateFile(env.ctx, file); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE page_annotations SET screenshot_file_id=$1`, shot)
	if _, err := svc.AddComment(managerCtx, pid, ids[1], manager.ID, "private-comment-marker"); err != nil {
		t.Fatal(err)
	}
	t.Run("hidden_content_and_no_mutation", func(t *testing.T) {
		comment, err := env.activitySvc.AddComment(managerCtx, taskdom.AddCommentInput{ProjectID: pid, TaskID: hidden.ID, ActorID: manager.ID, Content: json.RawMessage(`{"text":"private-task-comment"}`)})
		if err != nil {
			t.Fatal(err)
		}
		beforeEvents, err := env.redisClient.XLen(env.ctx, events.StreamPluginEvents).Result()
		if err != nil {
			t.Fatal(err)
		}
		beforeActivities := count(`SELECT count(*) FROM task_activities WHERE task_id=$1`, hidden.ID)
		for _, call := range []func() error{
			func() error {
				_, err := env.activitySvc.AddComment(ctx, taskdom.AddCommentInput{ProjectID: pid, TaskID: hidden.ID, ActorID: user.ID, Content: json.RawMessage(`{"text":"forbidden"}`)})
				return err
			},
			func() error {
				_, err := env.activitySvc.UpdateComment(ctx, comment.ID, pid, user.ID, nil, json.RawMessage(`{"text":"forbidden"}`))
				return err
			},
			func() error { return env.activitySvc.DeleteComment(ctx, comment.ID, pid, user.ID, nil) },
		} {
			if err := call(); !errors.Is(err, taskdom.ErrTaskNotFound) {
				t.Fatalf("hidden task comment mutation: %v", err)
			}
		}
		for _, list := range []func() ([]*annotationdom.PageAnnotation, error){
			func() ([]*annotationdom.PageAnnotation, error) { return svc.ListForPage(ctx, pid, eid, pfid, "/") },
			func() ([]*annotationdom.PageAnnotation, error) { return svc.ListForPortForward(ctx, pid, eid, pfid) },
		} {
			rows, err := list()
			if err != nil || len(rows) != 1 || rows[0].ID != ids[0] || len(rows[0].Comments) != 0 {
				t.Fatalf("hidden list/comments leaked: %+v %v", rows, err)
			}
		}
		before := store.calls
		for _, call := range []func() error{
			func() error { _, err := svc.Get(ctx, pid, ids[1]); return err },
			func() error { _, err := svc.GetScreenshotURL(ctx, pid, ids[1]); return err },
			func() error { _, err := svc.Resolve(ctx, pid, ids[1], manager.ID); return err },
			func() error { _, err := svc.Reopen(ctx, pid, ids[1]); return err },
			func() error { _, err := svc.AddComment(ctx, pid, ids[1], manager.ID, "forbidden"); return err },
			func() error {
				_, err := svc.CompleteScreenshotUpload(ctx, pid, ids[1], replacement, user.ID)
				return err
			},
			func() error {
				_, err := svc.CreateTaskFromAnnotation(ctx, pid, ids[1], annotationdom.CreateTaskFromAnnotationInput{})
				return err
			},
		} {
			if err := call(); !errors.Is(err, annotationdom.ErrAnnotationNotFound) {
				t.Fatalf("hidden annotation should be not found: %v", err)
			}
		}
		if store.calls != before || count(`SELECT count(*) FROM page_annotation_comments WHERE annotation_id=$1`, ids[1]) != 1 || count(`SELECT count(*) FROM page_annotations WHERE id=$1 AND status='open' AND screenshot_file_id=$2`, ids[1], shot) != 1 {
			t.Fatal("denied annotation mutated state or storage")
		}
		f, err := env.attachmentRepo.FindFileByID(ctx, replacement)
		if err != nil || f.UploadStatus != attachmentdom.UploadStatusPending {
			t.Fatal("denied screenshot completed")
		}
		if _, err := env.activitySvc.ListActivities(ctx, pid, hidden.ID); !errors.Is(err, taskdom.ErrTaskNotFound) {
			t.Fatalf("hidden activity: %v", err)
		}
		afterEvents, err := env.redisClient.XLen(env.ctx, events.StreamPluginEvents).Result()
		if err != nil {
			t.Fatal(err)
		}
		if afterEvents != beforeEvents || count(`SELECT count(*) FROM task_activities WHERE task_id=$1`, hidden.ID) != beforeActivities {
			t.Fatal("denied related content changed events or activities")
		}
		comments, err := env.activitySvc.ListActivities(managerCtx, pid, hidden.ID)
		if err != nil || len(comments) != 1 || comments[0].ID != comment.ID || !strings.Contains(string(comments[0].Content), "private-task-comment") {
			t.Fatalf("comment was changed: %+v %v", comments, err)
		}
	})
	t.Run("preclaim_refusal", func(t *testing.T) {
		before := count(`SELECT count(*) FROM tasks WHERE project_id=$1`, pid)
		if _, err := svc.CreateTaskFromAnnotation(ctx, pid, ids[0], annotationdom.CreateTaskFromAnnotationInput{}); !errors.Is(err, taskdom.ErrTaskForbidden) {
			t.Fatalf("creation: %v", err)
		}
		if count(`SELECT count(*) FROM page_annotations WHERE id=$1 AND task_creation_claimed_at IS NULL AND task_id IS NULL`, ids[0]) != 1 || count(`SELECT count(*) FROM tasks WHERE project_id=$1`, pid) != before || count(`SELECT count(*) FROM task_attachments`) != 0 {
			t.Fatal("refused creation claimed, created or attached")
		}
	})
	t.Run("current_assignee_and_historical_creator_denied", func(t *testing.T) {
		exec(`INSERT INTO task_assignees(task_id,member_id) VALUES($1,$2)`, hidden.ID, member)
		got, err := svc.Get(ctx, pid, ids[1])
		if err != nil || len(got.Comments) != 1 || got.Comments[0].Body != "private-comment-marker" {
			t.Fatalf("assignee detail: %+v %v", got, err)
		}
		rows, more, err := svc.SearchInProject(ctx, pid, annotationdom.SearchFilter{Limit: &limit})
		if err != nil || !more || len(rows) != 1 || rows[0].ID != ids[1] {
			t.Fatalf("authorized pagination: %v %v %v", rows, more, err)
		}
		cursor := annotationdom.EncodeAnnotationCursor(rows[0])
		rows, more, err = svc.SearchInProject(ctx, pid, annotationdom.SearchFilter{Limit: &limit, Cursor: &cursor})
		if err != nil || more || len(rows) != 1 || rows[0].ID != ids[0] {
			t.Fatal("authorized cursor incorrect")
		}
		if _, err := svc.GetScreenshotURL(ctx, pid, ids[1]); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Resolve(ctx, pid, ids[1], manager.ID); err != nil {
			t.Fatal(err)
		}
		if count(`SELECT count(*) FROM page_annotations WHERE id=$1 AND resolved_by=$2`, ids[1], user.ID) != 1 {
			t.Fatal("resolve actor forged")
		}
		if _, err := svc.Reopen(ctx, pid, ids[1]); err != nil {
			t.Fatal(err)
		}
		c, err := svc.AddComment(ctx, pid, ids[1], manager.ID, "assignee-progress")
		if err != nil || c.CreatedBy != user.ID {
			t.Fatalf("comment actor: %+v %v", c, err)
		}
		if _, err := svc.CompleteScreenshotUpload(ctx, pid, ids[1], replacement, user.ID); err != nil {
			t.Fatal(err)
		}
		exec(`DELETE FROM task_assignees WHERE task_id=$1`, hidden.ID)
		exec(`UPDATE tasks SET creator_user_id=$1 WHERE id=$2`, user.ID, hidden.ID)
		if _, err := svc.Get(ctx, pid, ids[1]); !errors.Is(err, annotationdom.ErrAnnotationNotFound) {
			t.Fatalf("creator-only annotation read: %v", err)
		}
		for _, call := range []func() error{
			func() error { _, err := svc.Resolve(ctx, pid, ids[1], user.ID); return err },
			func() error { _, err := svc.Reopen(ctx, pid, ids[1]); return err },
			func() error { _, err := svc.AddComment(ctx, pid, ids[1], user.ID, "creator-only"); return err },
			func() error {
				_, err := svc.CompleteScreenshotUpload(ctx, pid, ids[1], replacement, user.ID)
				return err
			},
		} {
			if err := call(); !errors.Is(err, annotationdom.ErrAnnotationNotFound) {
				t.Fatalf("creator-only annotation progress: %v", err)
			}
		}
		exec(`UPDATE tasks SET creator_user_id=$1 WHERE id=$2`, manager.ID, hidden.ID)
		before := store.calls
		if _, err := svc.GetScreenshotURL(ctx, pid, ids[1]); !errors.Is(err, annotationdom.ErrAnnotationNotFound) || store.calls != before {
			t.Fatal("new screenshot signed after reassignment")
		}
	})
	t.Run("current_role_and_membership", func(t *testing.T) {
		exec(`INSERT INTO task_assignees(task_id,member_id) VALUES($1,$2)`, hidden.ID, member)
		var role uuid.UUID
		if err := env.db.Get(&role, `SELECT project_role_id FROM project_members WHERE id=$1`, member); err != nil {
			t.Fatal(err)
		}
		var perms string
		if err := env.db.Get(&perms, `SELECT permissions::text FROM project_roles WHERE id=$1`, role); err != nil {
			t.Fatal(err)
		}
		exec(`UPDATE project_roles SET permissions='["annotations.read"]' WHERE id=$1`, role)
		rows, _, err := svc.SearchInProject(ctx, pid, annotationdom.SearchFilter{})
		if err != nil || len(rows) != 1 || rows[0].ID != ids[0] {
			t.Fatal("role revoke leaked task annotation or hid unbound")
		}
		exec(`UPDATE project_roles SET permissions=$1::jsonb WHERE id=$2`, perms, role)
		exec(`UPDATE project_members SET deleted_at=now() WHERE id=$1`, member)
		if _, err := svc.Get(ctx, pid, ids[1]); !errors.Is(err, annotationdom.ErrAnnotationNotFound) {
			t.Fatal("removed member read annotation")
		}
		exec(`UPDATE project_members SET deleted_at=NULL WHERE id=$1`, member)
	})
	t.Run("manager_creation_and_screenshot_task_read", func(t *testing.T) {
		var managerMember uuid.UUID
		if err := env.db.Get(&managerMember, `SELECT id FROM project_members WHERE project_id=$1 AND user_id=$2`, pid, manager.ID); err != nil {
			t.Fatal(err)
		}
		a, err := svc.CreateTaskFromAnnotation(managerCtx, pid, ids[0], annotationdom.CreateTaskFromAnnotationInput{ReporterID: managerMember})
		if err != nil || a.TaskID == nil {
			t.Fatalf("manager create: %+v %v", a, err)
		}
		attach := attachmentsvc.New(env.attachmentRepo, env.taskSvc, nil, store, "test")
		rows, err := attach.ListTaskAttachments(managerCtx, pid, *a.TaskID)
		if err != nil || len(rows) != 1 || rows[0].CreatedBy == nil || *rows[0].CreatedBy != manager.ID {
			t.Fatalf("annotation screenshot did not link with user identity: %+v %v", rows, err)
		}
		if _, err := attach.GetDownloadURL(managerCtx, pid, *a.TaskID, rows[0].ID, time.Minute, false); err != nil {
			t.Fatal("trusted annotation attachment cannot download", err)
		}
		if _, err := svc.CreateTaskFromAnnotation(managerCtx, pid, ids[0], annotationdom.CreateTaskFromAnnotationInput{}); !errors.Is(err, annotationdom.ErrAnnotationAlreadyHasTask) {
			t.Fatalf("duplicate: %v", err)
		}
	})
	t.Run("screenshot_upload_actor_binding", func(t *testing.T) {
		session, err := svc.InitiateScreenshotUpload(ctx, pid, eid, pfid, annotationdom.InitiateScreenshotUploadInput{FileName: "shot.png", ContentType: "image/png", FileSize: 1, UploadedBy: manager.ID})
		if err != nil {
			t.Fatal(err)
		}
		f, err := env.attachmentRepo.FindFileByID(env.ctx, session.FileID)
		if err != nil || f.UploadedBy == nil || *f.UploadedBy != user.ID {
			t.Fatal("screenshot uploader input trusted")
		}
		a, err := svc.Create(ctx, pid, eid, pfid, annotationdom.CreateAnnotationInput{Body: "own screenshot", PagePath: "/", CreatedBy: manager.ID, ScreenshotFileID: &session.FileID})
		if err != nil || a.CreatedBy != user.ID {
			t.Fatalf("annotation creator input trusted: %+v %v", a, err)
		}
	})

}

// Only storage calls reached by these service paths are implemented; any
// unexpected storage operation fails rather than silently succeeding.
type boundaryStorage struct {
	storage.Client
	calls int
}

func (s *boundaryStorage) PresignPutObject(context.Context, string, string, string, time.Duration) (string, error) {
	s.calls++
	return "https://storage.invalid/upload", nil
}
func (s *boundaryStorage) PresignGetObject(context.Context, string, string, time.Duration, string) (string, error) {
	s.calls++
	return "https://storage.invalid/download", nil
}
func (s *boundaryStorage) GetObject(context.Context, string, string, int64) ([]byte, error) {
	s.calls++
	return []byte("content"), nil
}
func (s *boundaryStorage) InitiateMultipartUpload(context.Context, string, string, string, int64, int64, time.Duration) (*storage.MultipartUpload, error) {
	s.calls++
	return &storage.MultipartUpload{UploadID: "pending-parts"}, nil
}
func (s *boundaryStorage) CompleteMultipartUpload(context.Context, string, string, string, []storage.CompletedPart) error {
	s.calls++
	return nil
}

func TestTaskUploadBindingAndCurrentAccess(t *testing.T) {
	env := newE2EEnv(t)
	seedTaskMemberUser(t, env, "upload-manager", "password1")
	client, token := taskMemberLogin(t, env, "upload-manager", "password1")
	pid := uuid.MustParse(createProjectForTasksViaAPI(t, env, client, token))
	manager, _ := env.userRepo.FindByUsername(env.ctx, "upload-manager")
	managerCtx := authz.WithActorID(env.ctx, manager.ID)
	users := [2]uuid.UUID{}
	members := [2]uuid.UUID{}
	contexts := [2]context.Context{}
	for i, name := range []string{"upload-a", "upload-b"} {
		seedUser(t, env, name, "password1", name)
		u, err := env.userRepo.FindByUsername(env.ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		users[i] = u.ID
		members[i] = uuid.New()
		contexts[i] = authz.WithActorID(env.ctx, u.ID)
		if _, err := env.db.Exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type) SELECT $1,$2,$3,id,'human' FROM project_roles WHERE project_id=$2 AND role_name='Editor'`, members[i], pid, u.ID); err != nil {
			t.Fatal(err)
		}
	}
	tasks := [3]*taskdom.Task{}
	for i := range tasks {
		var err error
		tasks[i], err = env.taskSvc.CreateTask(managerCtx, taskdom.CreateTaskInput{ProjectID: pid, Title: "file task", AssigneeIDs: []uuid.UUID{members[i%2]}})
		if err != nil {
			t.Fatal(err)
		}
	}
	store := &boundaryStorage{}
	svc := attachmentsvc.New(env.attachmentRepo, env.taskSvc, attachmentsvc.NewDocOwnerChecker(pg.NewDocumentRepository(env.db)), store, "test")
	upload := func(ctx context.Context, taskID uuid.UUID, multipart bool) *attachmentdom.UploadSession {
		t.Helper()
		size := int64(1)
		if multipart {
			size = storage.MultipartThreshold
		}
		session, err := svc.InitiateUpload(ctx, pid, attachmentdom.InitiateUploadInput{TaskID: taskID, FileName: "data.txt", ContentType: "text/plain", FileSize: size, UploadedBy: manager.ID})
		if err != nil {
			t.Fatal(err)
		}
		return session
	}
	complete := func(ctx context.Context, taskID uuid.UUID, session *attachmentdom.UploadSession) (*attachmentdom.TaskAttachment, error) {
		in := attachmentdom.CompleteUploadInput{TaskID: taskID, FileID: session.FileID, CreatedBy: manager.ID}
		if session.IsMultipart {
			in.UploadID = &session.Multipart.UploadID
			in.Parts = []attachmentdom.CompletedPart{{PartNumber: 1, ETag: "part"}}
		}
		return svc.CompleteUpload(ctx, pid, in)
	}
	t.Run("denial_has_no_storage_or_database_side_effects", func(t *testing.T) {
		for i, ctx := range contexts {
			hidden := tasks[1-i].ID
			before := store.calls
			for _, call := range []func() error{
				func() error { _, err := svc.ListTaskAttachments(ctx, pid, hidden); return err },
				func() error {
					_, err := svc.GetDownloadURL(ctx, pid, hidden, uuid.New(), time.Minute, false)
					return err
				},
				func() error { _, _, err := svc.GetAttachmentContent(ctx, pid, hidden, uuid.New()); return err },
				func() error {
					_, err := svc.InitiateUpload(ctx, pid, attachmentdom.InitiateUploadInput{TaskID: hidden, FileName: "x", FileSize: 1, ContentType: "text/plain"})
					return err
				},
				func() error {
					_, err := svc.CompleteUpload(ctx, pid, attachmentdom.CompleteUploadInput{TaskID: hidden, FileID: uuid.New()})
					return err
				},
				func() error { return svc.DeleteTaskAttachment(ctx, pid, hidden, uuid.New()) },
			} {
				if err := call(); !errors.Is(err, taskdom.ErrTaskNotFound) {
					t.Fatalf("cross-user access: %v", err)
				}
			}
			if store.calls != before {
				t.Fatal("denial contacted storage")
			}
		}
		var n int
		if err := env.db.Get(&n, `SELECT count(*) FROM files`); err != nil || n != 0 {
			t.Fatal("denial persisted file")
		}
		missing := attachmentsvc.New(env.attachmentRepo, nil, nil, store, "test")
		if _, err := missing.ListTaskAttachments(contexts[0], pid, tasks[0].ID); !errors.Is(err, taskdom.ErrTaskNotFound) {
			t.Fatal("missing policy allowed")
		}
		if _, err := svc.ListTaskAttachments(context.Background(), pid, tasks[0].ID); !errors.Is(err, taskdom.ErrTaskNotFound) {
			t.Fatal("missing actor allowed")
		}
	})
	t.Run("exact_pending_binding_and_historical_creator_denied", func(t *testing.T) {
		session := upload(contexts[0], tasks[0].ID, true)
		f, err := env.attachmentRepo.FindFileByID(env.ctx, session.FileID)
		if err != nil || f.UploadedBy == nil || *f.UploadedBy != users[0] {
			t.Fatal("input forged persisted uploader")
		}
		before := store.calls
		for _, call := range []func() error{
			func() error { _, err := complete(managerCtx, tasks[0].ID, session); return err },  // visible same task, different uploader
			func() error { _, err := complete(contexts[0], tasks[2].ID, session); return err }, // own upload, another visible task
		} {
			if err := call(); !errors.Is(err, attachmentdom.ErrFileNotFound) {
				t.Fatalf("foreign binding completed: %v", err)
			}
		}
		originalKey := f.StorageKey
		for _, key := range []string{"tasks/" + tasks[0].ID.String() + "/" + uuid.New().String() + "/x", "docs/" + tasks[0].ID.String() + "/" + f.ID.String() + "/x", "annotations/" + f.ID.String() + "/x", "avatars/user/" + users[0].String() + "/x"} {
			if _, err := env.db.Exec(`UPDATE files SET storage_key=$1 WHERE id=$2`, key, f.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := complete(contexts[0], tasks[0].ID, session); !errors.Is(err, attachmentdom.ErrFileNotFound) {
				t.Fatalf("namespace completed: %v", err)
			}
		}
		if _, err := env.db.Exec(`UPDATE files SET storage_key=$1 WHERE id=$2`, originalKey, f.ID); err != nil {
			t.Fatal(err)
		}
		wrongID := "wrong"
		for _, in := range []attachmentdom.CompleteUploadInput{
			{TaskID: tasks[0].ID, FileID: f.ID},
			{TaskID: tasks[0].ID, FileID: f.ID, UploadID: &wrongID},
			{TaskID: tasks[0].ID, FileID: f.ID, UploadID: &session.Multipart.UploadID},
		} {
			if _, err := svc.CompleteUpload(contexts[0], pid, in); err == nil {
				t.Fatal("invalid multipart completed")
			}
		}
		f, err = env.attachmentRepo.FindFileByID(env.ctx, f.ID)
		if err != nil || f.UploadStatus != attachmentdom.UploadStatusPending || f.MultipartUploadID == nil || store.calls != before {
			t.Fatal("denied completion changed pending record or storage")
		}
		rows, err := env.attachmentRepo.ListTaskAttachments(env.ctx, tasks[0].ID)
		if err != nil || len(rows) != 0 {
			t.Fatal("denied completion linked attachment")
		}
		a, err := complete(contexts[0], tasks[0].ID, session)
		if err != nil || a.CreatedBy == nil || *a.CreatedBy != users[0] || store.calls != before+1 {
			t.Fatalf("assignee multipart: %+v %v", a, err)
		}
		if _, err := complete(contexts[0], tasks[0].ID, session); !errors.Is(err, attachmentdom.ErrUploadNotPending) {
			t.Fatalf("repeated completion: %v", err)
		}
		if _, err := svc.GetDownloadURL(contexts[0], pid, tasks[0].ID, a.ID, time.Minute, false); err != nil {
			t.Fatal(err)
		}
		if data, _, err := svc.GetAttachmentContent(contexts[0], pid, tasks[0].ID, a.ID); err != nil || string(data) != "content" {
			t.Fatal("assignee content unavailable", err)
		}
		// Historical creator attribution does not grant current task access.
		if _, err := env.db.Exec(`DELETE FROM task_assignees WHERE task_id=$1`, tasks[0].ID); err != nil {
			t.Fatal(err)
		}
		if _, err := env.db.Exec(`UPDATE tasks SET creator_user_id=$1 WHERE id=$2`, users[0], tasks[0].ID); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ListTaskAttachments(contexts[0], pid, tasks[0].ID); !errors.Is(err, taskdom.ErrTaskNotFound) {
			t.Fatalf("creator listed attachments: %v", err)
		}
		if err := svc.DeleteTaskAttachment(contexts[0], pid, tasks[0].ID, a.ID); !errors.Is(err, taskdom.ErrTaskNotFound) {
			t.Fatalf("creator removed attachment: %v", err)
		}
		if _, err := svc.InitiateUpload(contexts[0], pid, attachmentdom.InitiateUploadInput{TaskID: tasks[0].ID}); !errors.Is(err, taskdom.ErrTaskNotFound) {
			t.Fatalf("creator initiated upload: %v", err)
		}
		if _, err := env.db.Exec(`UPDATE tasks SET creator_user_id=$1 WHERE id=$2`, manager.ID, tasks[0].ID); err != nil {
			t.Fatal(err)
		}
		before = store.calls
		if _, err := svc.GetDownloadURL(contexts[0], pid, tasks[0].ID, a.ID, time.Minute, false); !errors.Is(err, taskdom.ErrTaskNotFound) || store.calls != before {
			t.Fatal("signed after revoke")
		}
		if _, _, err := svc.GetAttachmentContent(contexts[0], pid, tasks[0].ID, a.ID); !errors.Is(err, taskdom.ErrTaskNotFound) || store.calls != before {
			t.Fatal("content after revoke")
		}
		if err := svc.DeleteTaskAttachment(managerCtx, pid, tasks[0].ID, a.ID); err != nil {
			t.Fatal("manager delete", err)
		}
	})
	t.Run("pending_upload_rechecks_membership", func(t *testing.T) {
		session := upload(contexts[1], tasks[1].ID, false)
		if _, err := env.db.Exec(`UPDATE project_members SET deleted_at=now() WHERE id=$1`, members[1]); err != nil {
			t.Fatal(err)
		}
		before := store.calls
		if _, err := complete(contexts[1], tasks[1].ID, session); !errors.Is(err, taskdom.ErrTaskNotFound) || store.calls != before {
			t.Fatalf("revoked uploader: %v", err)
		}
		f, err := env.attachmentRepo.FindFileByID(env.ctx, session.FileID)
		if err != nil || f.UploadStatus != attachmentdom.UploadStatusPending {
			t.Fatal("revoked file completed")
		}
		if _, err := env.db.Exec(`UPDATE project_members SET deleted_at=NULL WHERE id=$1`, members[1]); err != nil {
			t.Fatal(err)
		}
		if _, err := complete(contexts[1], tasks[1].ID, session); err != nil {
			t.Fatal("restored uploader", err)
		}
		own := upload(managerCtx, tasks[1].ID, false)
		if _, err := complete(managerCtx, tasks[1].ID, own); err != nil {
			t.Fatal("manager own completion", err)
		}
	})
	t.Run("document_sibling_namespace_and_uploader", func(t *testing.T) {
		docID := uuid.New()
		if _, err := env.db.Exec(`INSERT INTO documents(id,project_id,title) VALUES($1,$2,'doc')`, docID, pid); err != nil {
			t.Fatal(err)
		}
		taskUpload := upload(contexts[0], tasks[2].ID, true)
		before := store.calls
		if _, err := svc.CompleteDocUpload(contexts[0], attachmentdom.DocCompleteUploadInput{ProjectID: pid, DocID: docID, FileID: taskUpload.FileID, UploadID: &taskUpload.Multipart.UploadID, Parts: []attachmentdom.CompletedPart{{PartNumber: 1, ETag: "part"}}}); !errors.Is(err, attachmentdom.ErrFileNotFound) || store.calls != before {
			t.Fatal("doc route completed task multipart", err)
		}
		session, err := svc.InitiateDocUpload(contexts[0], attachmentdom.DocUploadInput{DocID: docID, FileName: "doc.txt", ContentType: "text/plain", FileSize: 1, UploadedBy: manager.ID})
		if err != nil {
			t.Fatal(err)
		}
		input := attachmentdom.DocCompleteUploadInput{ProjectID: pid, DocID: docID, FileID: session.FileID}
		if _, err := svc.CompleteDocUpload(managerCtx, input); !errors.Is(err, attachmentdom.ErrFileNotFound) {
			t.Fatal("doc foreign uploader", err)
		}
		otherDoc := uuid.New()
		if _, err := env.db.Exec(`INSERT INTO documents(id,project_id,title) VALUES($1,$2,'other')`, otherDoc, pid); err != nil {
			t.Fatal(err)
		}
		input.DocID = otherDoc
		if _, err := svc.CompleteDocUpload(contexts[0], input); !errors.Is(err, attachmentdom.ErrFileNotFound) {
			t.Fatal("doc foreign binding", err)
		}
		input.DocID = docID
		if _, err := env.db.Exec(`UPDATE files SET storage_key=$1 WHERE id=$2`, "docs/"+docID.String()+"/"+uuid.New().String()+"/x", session.FileID); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.CompleteDocUpload(contexts[0], input); !errors.Is(err, attachmentdom.ErrFileNotFound) {
			t.Fatal("doc foreign file prefix", err)
		}
		if _, err := env.db.Exec(`UPDATE files SET storage_key=$1 WHERE id=$2`, "docs/"+docID.String()+"/"+session.FileID.String()+"/doc.txt", session.FileID); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.CompleteDocUpload(contexts[0], input); err != nil {
			t.Fatal("legitimate document completion", err)
		}
		f, err := env.attachmentRepo.FindFileByID(env.ctx, taskUpload.FileID)
		if err != nil || f.UploadStatus != attachmentdom.UploadStatusPending {
			t.Fatal("sibling mutated pending task upload")
		}
	})
}

func (s *boundaryStorage) OpenObject(context.Context, string, string, string) (*storage.Object, error) {
	return nil, errors.New("unused streaming storage")
}
