package e2e_test

import (
	attachmentdom "github.com/Paca-AI/api/internal/domain/attachment"
	"github.com/google/uuid"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestDocFileURLAndContentShareCurrentReadPolicy(t *testing.T) {
	env := newE2EEnv(t)
	startFileDelivery(t, env)
	seedTaskMemberUser(t, env, "policy-manager", "password1")
	mc, mt := taskMemberLogin(t, env, "policy-manager", "password1")
	pid := uuid.MustParse(createProjectForTasksViaAPI(t, env, mc, mt))
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	doc := uuid.New()
	exec(`INSERT INTO documents(visibility,id,project_id,title,content)VALUES('legacy_project',$1,$2,'policy','[]')`, doc, pid)
	f := &attachmentdom.File{ID: uuid.New(), Bucket: "paca-attachments-e2e", FileName: "policy-secret.txt", ContentType: "text/plain", FileSize: 14, UploadStatus: attachmentdom.UploadStatusUploaded, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	f.StorageKey = "docs/" + doc.String() + "/" + f.ID.String() + "/file"
	if err := env.attachmentRepo.CreateFile(env.ctx, f); err != nil {
		t.Fatal(err)
	}
	if err := env.fileStore.PutObject(env.ctx, f.Bucket, f.StorageKey, f.ContentType, []byte("policy fixture")); err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/projects/" + pid.String() + "/docs/" + doc.String() + "/files/" + f.ID.String()
	type actor struct {
		name   string
		client *http.Client
		token  string
		user   uuid.UUID
		want   int
	}
	actors := []actor{}
	emptyRole := uuid.New()
	exec(`INSERT INTO global_roles(id,name,permissions)VALUES($1,$2,'[]')`, emptyRole, "empty-"+uuid.NewString())
	for _, tc := range []struct {
		name, global, project string
		want                  int
	}{{"global_projects_read", `["projects.read"]`, `[]`, 200}, {"project_docs_read", `[]`, `["docs.read"]`, 200}, {"project_projects_read_only", `[]`, `["projects.read"]`, 403}, {"no_permissions", `[]`, `[]`, 403}} {
		username := "policy-" + tc.name
		seedUser(t, env, username, "password1", tc.name)
		u, _ := env.userRepo.FindByUsername(env.ctx, username)
		gr, pr := uuid.New(), uuid.New()
		exec(`INSERT INTO global_roles(id,name,permissions)VALUES($1,$2,$3::jsonb)`, gr, "global-"+uuid.NewString(), tc.global)
		if err := env.roleRepo.ReplaceUserRoles(env.ctx, u.ID, []uuid.UUID{gr}); err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO project_roles(id,project_id,role_name,permissions)VALUES($1,$2,$3,$4::jsonb)`, pr, pid, "project-"+uuid.NewString(), tc.project)
		exec(`INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type)VALUES($1,$2,$3,$4,'human')`, uuid.New(), pid, u.ID, pr)
		client, token := taskMemberLogin(t, env, username, "password1")
		actors = append(actors, actor{tc.name, client, token, u.ID, tc.want})
	}
	actors = append(actors, actor{name: "anonymous", client: &http.Client{Timeout: 10 * time.Second}, want: 401})
	for _, public := range []bool{false, true} {
		exec(`UPDATE projects SET is_public=$1 WHERE id=$2`, public, pid)
		visibility := "private"
		if public {
			visibility = "public"
		}
		for _, a := range actors {
			t.Run(visibility+"/"+a.name, func(t *testing.T) {
				for _, tc := range []struct{ method, suffix string }{{"GET", "/download-url"}, {"GET", "/content"}, {"HEAD", "/content"}} {
					waitFileGate(t, env)
					req := mustRequest(env.ctx, t, tc.method, env.base+base+tc.suffix, nil)
					if a.token != "" {
						req.Header.Set("Authorization", "Bearer "+a.token)
					}
					resp := mustDo(t, a.client, req)
					body, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					if err != nil {
						t.Fatal(err)
					}
					t.Logf("%s %s status=%d", tc.method, tc.suffix, resp.StatusCode)
					if resp.StatusCode != a.want {
						t.Errorf("status=%d want=%d", resp.StatusCode, a.want)
					}
					if a.want == 200 && tc.suffix == "/content" && tc.method == "GET" && string(body) != "policy fixture" {
						t.Error("missing real storage content")
					}
					if a.want != 200 && (resp.Header.Get("Content-Disposition") != "" || resp.Header.Get("Content-Range") != "") {
						t.Error("denied file metadata leaked")
					}
				}
			})
		}
	}
	// The same old login and stable URL lose the explicitly granted global path.
	global := actors[0]
	if err := env.roleRepo.ReplaceUserRoles(env.ctx, global.user, []uuid.UUID{emptyRole}); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"/download-url", "/content"} {
		waitFileGate(t, env)
		req := mustRequest(env.ctx, t, "GET", env.base+base+suffix, nil)
		req.Header.Set("Authorization", "Bearer "+global.token)
		resp := mustDo(t, global.client, req)
		resp.Body.Close()
		assertStatus(t, resp, 403)
	}
}
