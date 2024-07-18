package backends

import "database/sql"

type AthenaReader struct {
	DSN string
}

func (r *AthenaReader) ReadData() ([]map[string]any, error) {
	db, err := sql.Open("postgres", r.DSN)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query("SELECT * FROM alerts")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	results := make([]map[string]any, 0)
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}

		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}

		item := make(map[string]any)
		for i, col := range columns {
			item[col] = values[i]
		}
		results = append(results, item)
	}

	return results, nil
}
