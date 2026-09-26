//go:build !nx_onnx

package onnx

// IOSpec описывает вход/выход модели: имя тензора и его shape.
// Shape содержит размерности в формате ONNX (int64), -1 для динамических осей.
type IOSpec struct {
	Name  string
	Shape []int64
}

// Session — stub-версия. Все операции возвращают ErrNotBuilt.
type Session struct{}

// NewSession в stub-варианте всегда возвращает ErrNotBuilt.
func NewSession(modelPath string, inputs, outputs []IOSpec) (*Session, error) {
	return nil, ErrNotBuilt
}

// NewSessionFromBytes в stub-варианте всегда возвращает ErrNotBuilt.
func NewSessionFromBytes(onnxData []byte, inputs, outputs []IOSpec) (*Session, error) {
	return nil, ErrNotBuilt
}

// NewSessionFromBytesWithThreads в stub-варианте всегда возвращает ErrNotBuilt.
func NewSessionFromBytesWithThreads(onnxData []byte, inputs, outputs []IOSpec, threads int) (*Session, error) {
	return nil, ErrNotBuilt
}

// TryInit в stub-варианте всегда возвращает ErrNotBuilt.
func TryInit(libPath string) error { return ErrNotBuilt }

// Input возвращает nil в stub-варианте.
func (s *Session) Input(i int) []float32 { return nil }

// Output возвращает nil в stub-варианте.
func (s *Session) Output(i int) []float32 { return nil }

// Run в stub-варианте всегда возвращает ErrNotBuilt.
func (s *Session) Run() error { return ErrNotBuilt }

// Close в stub-варианте — no-op.
func (s *Session) Close() error { return nil }
