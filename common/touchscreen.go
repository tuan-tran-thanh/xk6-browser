/*
 *
 * xk6-browser - a browser automation extension for k6
 * Copyright (C) 2021 Load Impact
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as
 * published by the Free Software Foundation, either version 3 of the
 * License, or (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package common

import (
	"context"
	"fmt"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/input"
	k6common "go.k6.io/k6/js/common"

	"github.com/grafana/xk6-browser/api"
)

// Ensure Touchscreen implements the EventEmitter and api.Touchscreen interfaces.
var _ EventEmitter = &Touchscreen{}
var _ api.Touchscreen = &Touchscreen{}

// Touchscreen represents a touchscreen.
type Touchscreen struct {
	BaseEventEmitter

	ctx      context.Context
	session  cdpSession
	keyboard *Keyboard
}

func NewTouchscreen(ctx context.Context, session cdpSession, keyboard *Keyboard) *Touchscreen {
	t := Touchscreen{
		ctx:      ctx,
		session:  session,
		keyboard: keyboard,
	}
	return &t
}

func (t *Touchscreen) tap(x float64, y float64) error {
	action := input.DispatchTouchEvent(input.TouchStart, []*input.TouchPoint{{X: x, Y: y}}).
		WithModifiers(input.Modifier(t.keyboard.modifiers))
	if err := action.Do(cdp.WithExecutor(t.ctx, t.session)); err != nil {
		return err
	}
	action = input.DispatchTouchEvent(input.TouchEnd, []*input.TouchPoint{}).
		WithModifiers(input.Modifier(t.keyboard.modifiers))
	if err := action.Do(cdp.WithExecutor(t.ctx, t.session)); err != nil {
		return err
	}
	return nil
}

// Tap dispatches a tap start and tap end event.
func (t *Touchscreen) Tap(x float64, y float64) {
	rt := k6common.GetRuntime(t.ctx)
	if err := t.tap(x, y); err != nil {
		k6common.Throw(rt, fmt.Errorf("unable to tap: %w", err))
	}
}
