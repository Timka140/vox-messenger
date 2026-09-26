//go:build !nx_dfn

package dfnative

type Processor struct{}

func New(cfg Config) (*Processor, error) { return nil, ErrNotBuilt }

func (p *Processor) ProcessFrame(in, out []float32) (float32, error) { return 0, ErrNotBuilt }

func (p *Processor) SetAttenLim(limDb float32) {}

func (p *Processor) Close() error { return nil }
