import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import './i18n';
import '@xyflow/react/dist/style.css';
import './index.css';
import './store/uiStore';
import App from './App';

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
