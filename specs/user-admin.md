Implementation Plan - User Administration & Admin Promotion
Implement a user administration view accessible to administrators, allowing them to view all registered users, see their roles, and promote/demote users to/from administrator status.

Self-demotion protection will be implemented so an admin cannot accidentally revoke their own administrative privileges.

Proposed Changes
Database & Queries
[MODIFY] 
db/query.sql
Add ListUsers query:
sql

-- name: ListUsers :many
SELECT id, username, is_admin, created_at FROM users ORDER BY id ASC;
Add RemoveUserAdmin query:
sql

-- name: RemoveUserAdmin :exec
UPDATE users SET is_admin = 0 WHERE id = ?;
Run sqlc generate to regenerate internal/database/.
Backend Handlers & Routing
[NEW] 
internal/handlers/users.go
Create UsersPage (GET /admin/users): Ensures current user is admin, retrieves all users with ListUsers, renders users.html.
Create PromoteUserSubmit (POST /admin/users/{id}/promote): Ensures current user is admin, calls SetUserAdmin, flashes success, redirects to /admin/users.
Create DemoteUserSubmit (POST /admin/users/{id}/demote): Ensures current user is admin. Prevents demoting oneself (id == currentUser.ID). Calls RemoveUserAdmin, flashes success, redirects to /admin/users.
[MODIFY] 
internal/handlers/app.go
Register "users.html" in the pages template map in NewApp.
Register the routes in Routes():
GET /admin/users
POST /admin/users/{id}/promote
POST /admin/users/{id}/demote
Web Templates & UI
[NEW] 
web/templates/users.html
Renders a table of registered users:
Username & User ID
Role badge (Admin vs Member)
Registration date
Action buttons (Promote to Admin for members, Revoke Admin for other admins).
[MODIFY] 
web/templates/layout.html
Add a Users navigation link for admin users ({{if .CurrentUser.IsAdmin}}<a href="/admin/users" class="btn btn-secondary btn-sm">Users</a>{{end}}) alongside the existing Invites link.
Verification Plan
Automated Tests
Run go test ./... with new unit tests in handlers_test.go:
TestUserAdministration:
Non-admin user accessing GET /admin/users or POST /admin/users/{id}/promote is redirected to / with unauthorized error.
Admin user listing users sees all registered users.
Admin user promoting a regular user updates their is_admin status to 1.
Admin user attempting to demote themselves receives an error flash message and operation is prevented.
Admin user demoting another admin sets their is_admin status back to 0.
Manual Verification
Log in as admin, navigate to top nav Users, promote a user, log in as that promoted user to verify admin nav options appear.