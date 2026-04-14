import { ShareIcon, Trash2Icon, UserPlusIcon } from "lucide-react";
import { type FC, useId, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "react-query";
import { chatACL, setChatUserRole } from "#/api/queries/chats";
import type * as TypesGen from "#/api/typesGenerated";
import { Avatar } from "#/components/Avatar/Avatar";
import { Button } from "#/components/Button/Button";
import { Input } from "#/components/Input/Input";
import {
	Popover,
	PopoverContent,
	PopoverTrigger,
} from "#/components/Popover/Popover";
import { Spinner } from "#/components/Spinner/Spinner";

interface ChatSharePopoverProps {
	chatId: string;
}

export const ChatSharePopover: FC<ChatSharePopoverProps> = ({ chatId }) => {
	const inputId = useId();
	const queryClient = useQueryClient();
	const [username, setUsername] = useState("");
	const aclQuery = useQuery(chatACL(chatId));
	const addUserMutation = useMutation(setChatUserRole(queryClient));
	const removeUserMutation = useMutation(setChatUserRole(queryClient));

	const handleAdd = () => {
		const trimmed = username.trim();
		if (!trimmed) return;
		addUserMutation.mutate(
			{ chatId, userId: trimmed, role: "read" },
			{
				onSuccess: () => setUsername(""),
			},
		);
	};

	const handleRemove = (user: TypesGen.ChatACLUser) => {
		removeUserMutation.mutate({ chatId, userId: user.id, role: "" });
	};

	const users = aclQuery.data?.users ?? [];

	return (
		<Popover>
			<PopoverTrigger asChild>
				<Button
					size="icon"
					variant="subtle"
					className="h-7 w-7 text-content-secondary hover:text-content-primary"
					aria-label="Share chat"
				>
					<ShareIcon className="h-4 w-4" />
				</Button>
			</PopoverTrigger>
			<PopoverContent align="end" className="w-80 p-3">
				<h3 className="mb-2 text-sm font-medium text-content-primary">
					Share Chat
				</h3>
				<p className="mb-3 text-xs text-content-secondary">
					Add users who can view this chat (read-only).
				</p>
				<div className="mb-3 flex gap-2">
					<Input
						id={inputId}
						placeholder="Username"
						value={username}
						onChange={(e) => setUsername(e.target.value)}
						onKeyDown={(e) => {
							if (e.key === "Enter") {
								e.preventDefault();
								handleAdd();
							}
						}}
						className="h-8 text-xs"
						aria-label="Username to share with"
					/>
					<Button
						size="sm"
						variant="outline"
						onClick={handleAdd}
						disabled={!username.trim() || addUserMutation.isPending}
						className="h-8 shrink-0"
					>
						{addUserMutation.isPending ? (
							<Spinner className="h-3.5 w-3.5" loading />
						) : (
							<UserPlusIcon className="h-3.5 w-3.5" />
						)}
						Add
					</Button>
				</div>
				{addUserMutation.isError && (
					<p className="mb-2 text-xs text-content-destructive">
						Failed to add user. Check the username and try again.
					</p>
				)}
				{aclQuery.isLoading ? (
					<div className="flex items-center justify-center py-4">
						<Spinner className="h-4 w-4" loading />
					</div>
				) : users.length > 0 ? (
					<ul className="flex flex-col gap-1">
						{users.map((user) => (
							<li
								key={user.id}
								className="flex items-center gap-2 rounded-md px-2 py-1.5 text-xs hover:bg-surface-secondary"
							>
								<Avatar
									src={user.avatar_url}
									fallback={user.username.charAt(0).toUpperCase()}
									size="sm"
								/>
								<span className="flex-1 truncate text-content-primary">
									{user.username}
								</span>
								<span className="text-content-secondary">read</span>
								<Button
									size="icon"
									variant="subtle"
									className="h-6 w-6 shrink-0 text-content-secondary hover:text-content-destructive"
									onClick={() => handleRemove(user)}
									disabled={removeUserMutation.isPending}
									aria-label={`Remove ${user.username}`}
								>
									<Trash2Icon className="h-3.5 w-3.5" />
								</Button>
							</li>
						))}
					</ul>
				) : (
					<p className="py-2 text-center text-xs text-content-secondary">
						Not shared with anyone yet.
					</p>
				)}
			</PopoverContent>
		</Popover>
	);
};
