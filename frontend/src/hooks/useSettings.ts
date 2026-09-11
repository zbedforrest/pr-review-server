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

  // One scope serializes the per-section saves; the cancel keeps an older
  // GET from landing on top of the saved response.
  return useMutation({
    scope: { id: 'settings' },
    mutationFn: (settings: Partial<Settings>) => updateSettings(settings),
    onMutate: async () => {
      await queryClient.cancelQueries({ queryKey: ['settings'] });
    },
    onSuccess: (saved, sent) => {
      queryClient.setQueryData<Settings>(['settings'], saved);
      if ('admin_logins' in sent) queryClient.invalidateQueries({ queryKey: ['currentUser'] });
    },
    onError: (err) => {
      console.error('Error updating settings:', err);
    },
  });
}
