// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// TEMP: StrictMode removed for ZIndexLayer portal isolation experiment
import { createRoot } from 'react-dom/client'
import './index.css'
import '@/lib/i18n'
import App from './App.tsx'

document.documentElement.dataset.glass = 'true'

createRoot(document.getElementById('root')!).render(<App />)
