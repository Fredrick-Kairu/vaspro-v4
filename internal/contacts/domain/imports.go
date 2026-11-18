package domain

// ImportStatus is an import's status (data model §8).
type ImportStatus string

const (
	AwaitingUpload      ImportStatus = "awaiting_upload"
	Scanning            ImportStatus = "scanning"
	Validating          ImportStatus = "validating"
	Processing          ImportStatus = "processing"
	Completed           ImportStatus = "completed"
	CompletedWithErrors ImportStatus = "completed_with_errors"
	Failed              ImportStatus = "failed"
	Cancelled           ImportStatus = "cancelled"
	Expired             ImportStatus = "expired"
)

// MaxImportRows is the most rows one import reads (data model §10).
const MaxImportRows = 1_000_000

var importMoves = map[ImportStatus][]ImportStatus{
	AwaitingUpload: {Scanning, Cancelled, Expired},
	Scanning:       {Validating, Failed},
	Validating:     {Processing, Failed},
	Processing:     {Completed, CompletedWithErrors, Failed},
}

// CanMoveImport is data model §8's status table.
func CanMoveImport(from, to ImportStatus) bool {
	for _, s := range importMoves[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Final reports whether nothing follows s: finished_at and delete_after
// are set when an import reaches it.
func (s ImportStatus) Final() bool {
	_, moves := importMoves[s]
	return !moves
}
