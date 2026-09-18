package users

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// ListActivity returns a user's account and booking events, newest first.
//
// The three sources are local to this schema on purpose: admin-service has no
// grant on clinical tables, and this endpoint must stay that way even if a
// future caller asks for symptoms or a diagnosis.
func (r *Repository) ListActivity(ctx context.Context, userID uuid.UUID, page, perPage int) ([]ActivityEntry, int64, error) {
	if perPage <= 0 {
		perPage = 20
	}
	if page <= 0 {
		page = 1
	}
	offset := (page - 1) * perPage
	id := userID.String()

	const countQ = `
		SELECT COUNT(*) FROM (
			SELECT 1 FROM user_projection
			 WHERE user_id = $1 AND registered_at IS NOT NULL
			UNION ALL
			SELECT 1 FROM audit_logs
			 WHERE resource_type = 'user' AND resource_id = $2
			UNION ALL
			SELECT 1 FROM appointments_projection
			 WHERE doctor_id = $1 OR patient_id = $1
		) events`

	var total int64
	if err := r.pool.QueryRow(ctx, countQ, userID, id).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("users: count activity: %w", err)
	}

	const listQ = `
		SELECT occurred_at, kind, summary, reference_id FROM (
			SELECT registered_at AS occurred_at,
			       'user.registered'::text AS kind,
			       CASE role
			         WHEN 'doctor' THEN 'Registered as a doctor'
			         WHEN 'patient' THEN 'Registered as a patient'
			         ELSE 'Account registered'
			       END AS summary,
			       NULL::uuid AS reference_id
			  FROM user_projection
			 WHERE user_id = $1 AND registered_at IS NOT NULL
			UNION ALL
			SELECT created_at,
			       action,
			       CASE
			         WHEN COALESCE(new_value->>'reason', '') <> '' THEN
			           CASE action
			             WHEN 'user.suspend_requested' THEN 'Suspension requested'
			             WHEN 'user.reinstate_requested' THEN 'Reinstatement requested'
			             ELSE replace(action, '.', ' ')
			           END || ': ' || left(new_value->>'reason', 200)
			         ELSE
			           CASE action
			             WHEN 'user.suspend_requested' THEN 'Suspension requested'
			             WHEN 'user.reinstate_requested' THEN 'Reinstatement requested'
			             ELSE replace(action, '.', ' ')
			           END
			       END,
			       NULL::uuid
			  FROM audit_logs
			 WHERE resource_type = 'user' AND resource_id = $2
			UNION ALL
			SELECT COALESCE(scheduled_at, occurred_at),
			       'appointment.' || status,
			       CASE status
			         WHEN 'created' THEN 'Appointment booked'
			         WHEN 'confirmed' THEN 'Appointment confirmed'
			         WHEN 'cancelled' THEN 'Appointment cancelled'
			         WHEN 'completed' THEN 'Appointment completed'
			         WHEN 'no_show' THEN 'Appointment marked no-show'
			         ELSE 'Appointment ' || status
			       END
			       || CASE
			            WHEN specialty_code IS NOT NULL AND specialty_code <> ''
			              THEN ' (' || specialty_code || ')'
			            ELSE ''
			          END,
			       appointment_id
			  FROM appointments_projection
			 WHERE doctor_id = $1 OR patient_id = $1
		) events
		ORDER BY occurred_at DESC
		LIMIT $3 OFFSET $4`

	rows, err := r.pool.Query(ctx, listQ, userID, id, perPage, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("users: list activity: %w", err)
	}
	defer rows.Close()

	out := make([]ActivityEntry, 0)
	for rows.Next() {
		var e ActivityEntry
		if err := rows.Scan(&e.OccurredAt, &e.Kind, &e.Summary, &e.ReferenceID); err != nil {
			return nil, 0, fmt.Errorf("users: scan activity: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("users: list activity: %w", err)
	}
	return out, total, nil
}

func registrationSummary(role string) string {
	switch role {
	case "doctor":
		return "Registered as a doctor"
	case "patient":
		return "Registered as a patient"
	default:
		return "Account registered"
	}
}

func auditSummary(action, reason string) string {
	label := auditLabel(action)
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return label
	}
	if len(reason) > 200 {
		reason = reason[:200]
	}
	return label + ": " + reason
}

func auditLabel(action string) string {
	switch action {
	case "user.suspend_requested":
		return "Suspension requested"
	case "user.reinstate_requested":
		return "Reinstatement requested"
	default:
		return strings.ReplaceAll(action, ".", " ")
	}
}

func appointmentSummary(status, specialty string) string {
	var label string
	switch status {
	case "created":
		label = "Appointment booked"
	case "confirmed":
		label = "Appointment confirmed"
	case "cancelled":
		label = "Appointment cancelled"
	case "completed":
		label = "Appointment completed"
	case "no_show":
		label = "Appointment marked no-show"
	default:
		label = "Appointment " + status
	}
	if specialty != "" {
		return label + " (" + specialty + ")"
	}
	return label
}
