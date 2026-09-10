import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { fetchSettings, updateSettings, type Settings } from '@/api/settings';

export function useSettings() {
  return useQuery({
    queryKey: ['settings'],
    queryFn: fetchSettings,
    staleTime: 5 * 60 * 1000, // 5 minutes
  });
}

export function useSettingsEditor() {
  return useQuery({
    queryKey: ['settings'],
    queryFn: fetchSettings,
    refetchOnMount: 'always',
  });
}

export function useUpdateSettings() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: (settings: Partial<Settings>) => updateSettings(settings),
    onSuccess: (saved) => {
      queryClient.setQueryData<Settings>(['settings'], saved);
    },
    onError: (err) => {
      console.error('Error updating settings:', err);
    },
  });
}
