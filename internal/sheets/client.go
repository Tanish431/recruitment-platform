package sheets

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

type Client struct {
	svc     *sheets.Service
	sheetID string
}

func New(ctx context.Context, credentialsPath, sheetID string) (*Client, error) {
	svc, err := sheets.NewService(ctx, option.WithCredentialsFile(credentialsPath))
	if err != nil {
		return nil, err
	}
	return &Client{svc: svc, sheetID: sheetID}, nil
}

// ReadRows reads every row from the given tab (raw values, header row included).
func (c *Client) ReadRows(ctx context.Context, tabName string) ([][]interface{}, error) {
	resp, err := c.svc.Spreadsheets.Values.Get(c.sheetID, tabName).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return resp.Values, nil
}

func (c *Client) AppendRow(ctx context.Context, tabName string, row []interface{}) error {
	vr := &sheets.ValueRange{Values: [][]interface{}{row}}
	_, err := c.svc.Spreadsheets.Values.Append(c.sheetID, tabName+"!A1", vr).
		ValueInputOption("RAW").
		InsertDataOption("INSERT_ROWS").
		Context(ctx).
		Do()
	return err
}

// LogEvent stays append-only - used for the query/swap audit trail, where
// history matters more than "current state."
func (c *Client) LogEvent(ctx context.Context, tabName, candidateEmail, eventType string, details ...interface{}) error {
	row := []interface{}{time.Now().Format(time.RFC3339), candidateEmail, eventType}
	row = append(row, details...)
	return c.AppendRow(ctx, tabName, row)
}

// UpsertRow finds the row whose column A matches keyValue (candidate email)
// and overwrites it; if no match exists, appends a new row instead. This is
// what keeps the roster tabs as "one row per candidate, always current"
// rather than a growing log.
func (c *Client) UpsertRow(ctx context.Context, tabName, keyValue string, row []interface{}) error {
	return c.UpsertRowAtColumn(ctx, tabName, "A", keyValue, row)
}

// UpsertRowAtColumn is UpsertRow but lets the caller specify which column
// holds the match key, for tabs where the key isn't in column A.
func (c *Client) UpsertRowAtColumn(ctx context.Context, tabName, keyColumn, keyValue string, row []interface{}) error {
	resp, err := c.svc.Spreadsheets.Values.Get(c.sheetID, fmt.Sprintf("%s!%s:%s", tabName, keyColumn, keyColumn)).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("failed to read existing rows: %w", err)
	}

	rowIndex := -1
	for i, v := range resp.Values {
		if len(v) > 0 && fmt.Sprint(v[0]) == keyValue {
			rowIndex = i + 1
			break
		}
	}

	if rowIndex == -1 {
		return c.AppendRow(ctx, tabName, row)
	}

	rangeStr := fmt.Sprintf("%s!A%d", tabName, rowIndex)
	vr := &sheets.ValueRange{Values: [][]interface{}{row}}
	_, err = c.svc.Spreadsheets.Values.Update(c.sheetID, rangeStr, vr).
		ValueInputOption("RAW").
		Context(ctx).
		Do()
	return err
}
