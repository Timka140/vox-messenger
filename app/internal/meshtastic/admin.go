package meshtastic

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	meshpb "vox_app/internal/meshtastic/meshpb"
)

// SendAdmin отправляет Meshtastic AdminMessage на указанный узел. Для локальной
// конфигурации устройства dst = a.LocalNode(). Сессионный ключ (SessionPasskey)
// требуется только для удалённого администрирования — локальные пакеты проходят
// без него.
func (a *Adapter) SendAdmin(ctx context.Context, dst uint32, msg *meshpb.AdminMessage) error {
	select {
	case <-a.done:
		return ErrClosed
	default:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if msg == nil {
		return fmt.Errorf("admin message is nil")
	}

	body, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal admin: %w", err)
	}

	pkt := &meshpb.ToRadio{
		PayloadVariant: &meshpb.ToRadio_Packet{
			Packet: &meshpb.MeshPacket{
				To:       dst,
				Channel:  0,
				HopLimit: a.cfg.HopLimit,
				WantAck:  true,
				PayloadVariant: &meshpb.MeshPacket_Decoded{
					Decoded: &meshpb.Data{
						Portnum: meshpb.PortNum_ADMIN_APP,
						Payload: body,
						// Критично для Get-запросов: без WantResponse=true прошивка
						// Meshtastic молча проглатывает admin-пакет (он логируется,
						// но ответ не отправляется). С флагом — AdminModule шлёт
						// обратный MeshPacket с соответствующим *Response-вариантом.
						WantResponse: true,
					},
				},
			},
		},
	}
	if err := a.writeToRadio(pkt); err != nil {
		return fmt.Errorf("write admin: %w", err)
	}
	a.log.Debug("sent admin", "dst", fmt.Sprintf("%#x", dst), "bytes", len(body))
	return nil
}

// QueryDeviceConnectionStatus отправляет AdminMessage.GetDeviceConnectionStatusRequest
// на свой узел и ждёт ответ на admin-канале с указанным таймаутом. Возвращает
// DeviceConnectionStatus (содержит WiFi/Ethernet/BT/Serial status с IP и SSID).
//
// Не совпадающие по типу admin-сообщения дропаются (first-matching-response wins) —
// этого достаточно, пока мы не шлём несколько admin-запросов одновременно.
func (a *Adapter) QueryDeviceConnectionStatus(ctx context.Context, timeout time.Duration) (*meshpb.DeviceConnectionStatus, error) {
	dst := a.LocalNode()
	if dst == 0 {
		return nil, errors.New("local node unknown")
	}
	msg := &meshpb.AdminMessage{
		PayloadVariant: &meshpb.AdminMessage_GetDeviceConnectionStatusRequest{
			GetDeviceConnectionStatusRequest: true,
		},
	}
	if err := a.SendAdmin(ctx, dst, msg); err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}
	deadline := time.After(timeout)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-a.done:
			return nil, ErrClosed
		case <-deadline:
			return nil, errors.New("timeout waiting for admin response")
		case am, ok := <-a.admin:
			if !ok {
				return nil, ErrClosed
			}
			if resp := am.GetGetDeviceConnectionStatusResponse(); resp != nil {
				return resp, nil
			}
			// Чужой admin-ответ, ждём ещё.
		}
	}
}

// SendAdminBatch оборачивает несколько AdminMessage-изменений в транзакцию
// begin_edit_settings / commit_edit_settings. Прошивка Meshtastic отложит
// применение (и последующий reboot) до commit — все SetConfig улетят одним
// кадром, устройство перезагрузится ровно один раз.
//
// Если changes пусто — функция no-op (ни begin, ни commit не шлются).
// При ошибке любого шага commit НЕ отправляется — прошивка сама откатит
// после таймаута транзакции (комментарий в proto).
func (a *Adapter) SendAdminBatch(ctx context.Context, dst uint32, changes []*meshpb.AdminMessage) error {
	if len(changes) == 0 {
		return nil
	}
	begin := &meshpb.AdminMessage{
		PayloadVariant: &meshpb.AdminMessage_BeginEditSettings{BeginEditSettings: true},
	}
	if err := a.SendAdmin(ctx, dst, begin); err != nil {
		return fmt.Errorf("begin_edit_settings: %w", err)
	}
	for i, ch := range changes {
		if ch == nil {
			continue
		}
		if err := a.SendAdmin(ctx, dst, ch); err != nil {
			return fmt.Errorf("batch step %d: %w", i+1, err)
		}
	}
	commit := &meshpb.AdminMessage{
		PayloadVariant: &meshpb.AdminMessage_CommitEditSettings{CommitEditSettings: true},
	}
	if err := a.SendAdmin(ctx, dst, commit); err != nil {
		return fmt.Errorf("commit_edit_settings: %w", err)
	}
	return nil
}

// QueryConfig отправляет AdminMessage.GetConfigRequest(type) и ждёт ответ
// (GetConfigResponse) на admin-канале. Возвращает распарсенный *Config.
// Используется для достоверного чтения применённой конфигурации —
// DeviceConnectionStatus содержит только runtime-состояние, но не то,
// что выставлено в конфиге (enabled/mode и т.д.).
func (a *Adapter) QueryConfig(ctx context.Context, timeout time.Duration, typ meshpb.AdminMessage_ConfigType) (*meshpb.Config, error) {
	dst := a.LocalNode()
	if dst == 0 {
		return nil, errors.New("local node unknown")
	}
	msg := &meshpb.AdminMessage{
		PayloadVariant: &meshpb.AdminMessage_GetConfigRequest{
			GetConfigRequest: typ,
		},
	}
	if err := a.SendAdmin(ctx, dst, msg); err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}
	deadline := time.After(timeout)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-a.done:
			return nil, ErrClosed
		case <-deadline:
			return nil, errors.New("timeout waiting for admin response")
		case am, ok := <-a.admin:
			if !ok {
				return nil, ErrClosed
			}
			if resp := am.GetGetConfigResponse(); resp != nil {
				return resp, nil
			}
			// Чужой admin-ответ, ждём ещё.
		}
	}
}

// IPv4FromUint32 декодирует IP из uint32 как его кладёт Meshtastic-прошивка.
// Эмпирически проверено на Heltec V3: raw=0xab1fa8c0 → 192.168.31.171 в
// Xiaomi-сети. Значит порядок little-endian: младший байт = первый октет.
func IPv4FromUint32(raw uint32) string {
	if raw == 0 {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.%d",
		byte(raw), byte(raw>>8), byte(raw>>16), byte(raw>>24))
}
