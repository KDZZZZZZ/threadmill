import { createBrowserRouter } from 'react-router-dom'
import { WorkspacePage } from '@/pages/workspace-page'

export const router = createBrowserRouter([{ path: '/', element: <WorkspacePage /> }])
