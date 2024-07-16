package backends

import "database/sql"

type SnowflakeReader struct {
	DSN string
}

func (r *SnowflakeReader) ReadData() ([]map[string]interface{}, error) {
	db, err := sql.Open("snowflake", r.DSN)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query("SELECT * FROM your_table")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	results := make([]map[string]interface{}, 0)
	for rows.Next() {
		values := make([]interface{}, len(columns))
		pointers := make([]interface{}, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}

		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}

		item := make(map[string]interface{})
		for i, col := range columns {
			item[col] = values[i]
		}
		results = append(results, item)
	}

	return results, nil
}
