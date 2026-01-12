import { create } from 'zustand';
import { devtools, persist } from 'zustand/middleware';

// UI Store - persisted in localStorage
interface UIStore {
  priorityQueueCollapsed: boolean;
  togglePriorityQueue: () => void;
}

export const useUIStore = create<UIStore>()(
  persist(
    devtools(
      (set) => ({
        priorityQueueCollapsed: true, // Default: collapsed
        togglePriorityQueue: () =>
          set((state) => ({
            priorityQueueCollapsed: !state.priorityQueueCollapsed,
          })),
      }),
      { name: 'UIStore' }
    ),
    {
      name: 'pr-dashboard-ui',
    }
  )
);
