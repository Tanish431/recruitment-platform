package router

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Tanish431/recruitment-platform/internal/auth"
	"github.com/Tanish431/recruitment-platform/internal/config"
	"github.com/Tanish431/recruitment-platform/internal/handlers"
	appmiddleware "github.com/Tanish431/recruitment-platform/internal/middleware"
	"github.com/Tanish431/recruitment-platform/internal/sheets"
)

func New(cfg *config.Config, pool *pgxpool.Pool, sheetsClient *sheets.Client) http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "http://localhost:3000")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusOK)
				return
			}
			next.ServeHTTP(w, r)
		})
	})

	r.Use(chimiddleware.Recoverer)
	r.Use(appmiddleware.Logging)

	oauthCfg := auth.NewOAuthConfig(cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.GoogleRedirectURL)
	authHandler := handlers.NewAuthHandler(pool, oauthCfg, cfg.AllowedEmailDomain, cfg.FrontendURL, sheetsClient)
	authMW := appmiddleware.NewAuthMiddleware(pool)

	adminHandler := handlers.NewAdminHandler(pool, cfg.AllowedEmailDomain, sheetsClient)
	assignmentHandler := handlers.NewAssignmentHandler(pool, sheetsClient)
	evalHandler := handlers.NewEvaluationHandler(pool, sheetsClient)
	round2Handler := handlers.NewRound2Handler(pool, sheetsClient)
	candidateHandler := handlers.NewCandidateHandler(pool, sheetsClient)
	queryHandler := handlers.NewQueryHandler(pool, sheetsClient)
	propertyHandler := handlers.NewPropertyHandler(pool)

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	// Auth
	r.Get("/auth/login", authHandler.Login)
	r.Get("/auth/callback", authHandler.Callback)
	r.Post("/auth/logout", authHandler.Logout)

	// Authenticated, candidate-facing
	r.Group(func(r chi.Router) {
		r.Use(authMW.RequireAuth)
		r.Get("/me", authHandler.Me)
		r.Patch("/me/profile", authHandler.UpdateProfile)
		r.Delete("/me/queries/{queryID}", candidateHandler.CancelQuery)
		r.Get("/me/assignment", candidateHandler.MyAssignments)
		r.Post("/me/queries", candidateHandler.RaiseQuery)
		r.Post("/me/unavailability", candidateHandler.SubmitUnavailability)
		r.Get("/me/unavailability", candidateHandler.MyUnavailability)
		r.Get("/rounds", adminHandler.ListRounds)
		r.Get("/rounds/active", adminHandler.ActiveRound)
		r.Get("/rounds/{roundID}/properties", propertyHandler.ListProperties)
		r.Post("/me/acknowledge-result", candidateHandler.AcknowledgeResult)
		r.Get("/locations", adminHandler.ListLocations)
	})

	// Admin - locked to role=admin
	r.Route("/admin", func(r chi.Router) {
		r.Use(authMW.RequireAuth)
		r.Use(func(next http.Handler) http.Handler {
			return appmiddleware.RequireRole(next, "admin")
		})
		r.Patch("/slots/{slotID}/location", adminHandler.UpdateSlotLocation)
		r.Post("/candidates/import-from-sheet", adminHandler.ImportFromSheet)
		r.Patch("/slots/{slotID}/capacity", adminHandler.UpdateSlotCapacity)
		r.Post("/slots", adminHandler.CreateSlot)
		r.Get("/slots", adminHandler.ListSlots)
		r.Delete("/slots/{slotID}", adminHandler.DeleteSlot)
		r.Post("/slots/generate-schedule", adminHandler.GenerateSchedule)
		r.Post("/rounds/{roundID}/activate", adminHandler.ActivateRound)
		r.Post("/rounds/deactivate", adminHandler.DeactivateAllRounds)
		r.Post("/judges/promote", adminHandler.PromoteToJudge)
		r.Get("/rounds/{roundID}/unavailability", adminHandler.ListUnavailability)
		r.Post("/rounds/{roundID}/assign", assignmentHandler.RunAssignment)
		r.Get("/rounds/{roundID}/assignments", adminHandler.ListAssignments)
		r.Post("/assignments/{assignmentID}/move", adminHandler.MoveAssignment)
		r.Post("/assignments/swap", adminHandler.SwapAssignments)
		r.Get("/users/search", adminHandler.SearchUsers)
		r.Get("/queries", queryHandler.ListPending)
		r.Post("/queries/{queryID}/resolve", queryHandler.Resolve)
		r.Get("/queries/open-slots", queryHandler.OpenSlotsForRound)
		r.Get("/queries/other-assignments", queryHandler.OtherAssignmentsForRound)
		r.Delete("/queries/{queryID}", queryHandler.AdminCancelQuery)
		r.Get("/locations", adminHandler.ListLocations)
		r.Post("/locations", adminHandler.CreateLocation)
		r.Get("/candidates", adminHandler.ListCandidates)
		r.Delete("/assignments/{assignmentID}/unassign", adminHandler.UnassignCandidate)
		r.Post("/slots/{slotID}/candidates", adminHandler.AddCandidateToSlot)
		r.Get("/rounds/{roundID}/unassigned-candidates", adminHandler.ListUnassignedCandidates)
		r.Post("/rounds/{roundID}/sync-results", adminHandler.SyncRoundResultsFromSheet)
		r.Post("/rounds/{roundID}/properties", propertyHandler.CreateProperty)
		r.Delete("/properties/{propertyID}", propertyHandler.DeleteProperty)
		r.Post("/slots/{slotID}/reassign-judge", adminHandler.ReassignSlotJudge)
	})

	// Judge - locked to role=judge or role=admin
	r.Route("/judge", func(r chi.Router) {
		r.Use(authMW.RequireAuth)
		r.Use(func(next http.Handler) http.Handler {
			return appmiddleware.RequireRole(next, "judge", "admin")
		})

		// Round 2 - slot claiming
		r.Post("/slots/claim", round2Handler.ClaimSlot)
		r.Get("/slots/available", round2Handler.AvailableSlots)
		r.Get("/slots/my-claimed", round2Handler.MyClaimedSlots)
		r.Post("/slots/{slotID}/close", round2Handler.CloseSlot)
		r.Get("/slots/open-to-join", round2Handler.OpenSlotsToJoin)
		r.Post("/slots/{slotID}/join", round2Handler.JoinSlot)
		r.Get("/slots/{slotID}/co-judge-status", round2Handler.SlotCoJudgeStatus)
		r.Post("/slots/{slotID}/mark-co-judge-present", round2Handler.MarkCoJudgePresent)
		r.Post("/slots/{slotID}/mark-host-present", round2Handler.MarkHostPresent)
		r.Post("/slots/{slotID}/prep", round2Handler.SetTeamPrep)
		// Round 1 / 3 - live queue
		r.Get("/queue", evalHandler.Queue)
		r.Post("/evaluations/{id}/checkin", evalHandler.CheckIn)
		r.Post("/evaluations/{id}/claim", evalHandler.Claim)
		r.Post("/evaluations/{id}/submit", evalHandler.Submit)
		r.Post("/evaluations/{id}/skip", evalHandler.Skip)
		r.Post("/evaluations/{id}/noshow", evalHandler.NoShow)
		r.Get("/users/search", adminHandler.SearchUsers)
		r.Get("/lookup", evalHandler.LookupByEmail)
		r.Post("/evaluations/{id}/properties/{propertyID}/rating", propertyHandler.RateEvaluationProperty)
		r.Get("/evaluations/{id}/property-ratings", propertyHandler.EvaluationRatings)
		r.Post("/participants/{id}/properties/{propertyID}/rating", propertyHandler.RateParticipantProperty)
		r.Get("/participants/{id}/property-ratings", propertyHandler.ParticipantRatings)

		// Round 2 - scoring the slot they claimed
		r.Get("/slots/{slotID}/participants", round2Handler.Participants)
		r.Post("/participants/{id}/attendance", round2Handler.SetAttendance)
		r.Post("/participants/{id}/score", round2Handler.SetScore)
	})

	return r
}
