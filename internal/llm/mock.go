package llm

import "context"

type Mock struct {
	Response string
	Err      error
}

func NewMock(response string) *Mock {
	return &Mock{Response: response}
}

func (m *Mock) Generate(_ context.Context, _ string) (string, error) {
	if m.Err != nil {
		return "", m.Err
	}
	return m.Response, nil
}

func (m *Mock) Describe(_ context.Context, _ []byte) (string, error) {
	if m.Err != nil {
		return "", m.Err
	}
	return m.Response, nil
}
